package main

import (
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

const (
	defaultProductsFile = "products.yaml"
	defaultDatabaseFile = "pos.db"
	debugDatabaseFile   = "pos-debug.db"
	testDatabaseFile    = "pos-test.db"
	defaultEventsDir    = "events"
)

const usageText = `使い方:
  go run . --deploy  # 本番モード: pos.db に売上・在庫を永続保存
  go run . --debug   # デバッグモード: 終了時に pos-debug.db を初期化
  go run . --test    # テストモード: 終了時に pos-test.db を初期化
`

type runConfig struct {
	Mode                    string
	DatabaseFile            string
	ResetOnShutdown         bool
	ExportCatalogOnShutdown bool
	SelectDatabase          bool
}

type ProductCatalog struct {
	Store        Store         `yaml:"store" json:"store"`
	Products     []Product     `yaml:"products" json:"products"`
	SetDiscounts []SetDiscount `yaml:"set_discounts" json:"set_discounts"`
	Audit        AuditSettings `yaml:"audit" json:"audit"`
}

type AuditSettings struct {
	OutputDirectory string `yaml:"output_directory" json:"output_directory"`
}

type Store struct {
	Name     string `yaml:"name" json:"name"`
	Currency string `yaml:"currency" json:"currency"`
}

type Product struct {
	ID                    string       `yaml:"id" json:"id"`
	Name                  string       `yaml:"name" json:"name"`
	UnitPrice             int          `yaml:"unit_price" json:"unit_price"`
	InitialStock          int          `yaml:"initial_stock" json:"initial_stock"`
	DeprecatedSetDiscount *SetDiscount `yaml:"set_discount,omitempty" json:"-"`
}

type SetDiscount struct {
	ID       string            `yaml:"id" json:"id"`
	Name     string            `yaml:"name" json:"name"`
	Items    []SetDiscountItem `yaml:"items" json:"items"`
	SetPrice int               `yaml:"set_price" json:"set_price"`
}

type SetDiscountItem struct {
	ProductID string `yaml:"product_id" json:"product_id"`
	Quantity  int    `yaml:"quantity" json:"quantity"`
}

type App struct {
	catalogMu sync.RWMutex
	catalog   ProductCatalog
	byID      map[string]Product
	db        *sql.DB
	eventName string
}

type salesSummaryItem struct {
	ProductID string
	Name      string
	Quantity  int
}

type operationAuditEntry struct {
	Category    string
	Action      string
	SubjectType string
	SubjectID   string
	Details     any
}

type catalogProductRequest struct {
	Name      string `json:"name"`
	UnitPrice int    `json:"unit_price"`
	Stock     int    `json:"stock"`
}

type setDiscountsRequest struct {
	SetDiscounts []SetDiscount `json:"set_discounts"`
}

type auditSettingsRequest struct {
	OutputDirectory string `json:"output_directory"`
}

type auditExportResponse struct {
	Path string `json:"path"`
}

type eventRecord struct {
	ID             string `json:"id"`
	DatabaseFile   string `json:"-"`
	ProductsFile   string `json:"-"`
	PersistCatalog bool   `json:"-"`
}

type eventListResponse struct {
	Events []eventRecord `json:"events"`
}

type eventSelectionResponse struct {
	Selected bool `json:"selected"`
}

type eventRequest struct {
	ID string `json:"id"`
}

type eventManager struct {
	mu               sync.RWMutex
	app              *App
	selected         eventRecord
	config           runConfig
	productsTemplate string
	eventsDirectory  string
}

var catalogIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type inventoryItem struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	Quantity  int    `json:"quantity"`
}

type inventoryResponse struct {
	Items []inventoryItem `json:"items"`
}

type inventoryUpdateRequest struct {
	Quantity int `json:"quantity"`
}

type insufficientStockError struct {
	ProductName string
}

func (e *insufficientStockError) Error() string {
	return fmt.Sprintf("在庫が不足しています: %s", e.ProductName)
}

type checkoutRequest struct {
	Items                  []checkoutItem `json:"items"`
	CorrectionAdjustmentID *int64         `json:"correction_adjustment_id,omitempty"`
}

type checkoutItem struct {
	ProductID string `json:"product_id"`
	Quantity  int    `json:"quantity"`
}

type checkoutResponse struct {
	SaleID      int64             `json:"sale_id"`
	Subtotal    int               `json:"subtotal"`
	Discount    int               `json:"discount"`
	Total       int               `json:"total"`
	Items       []saleLine        `json:"items"`
	AppliedSets []appliedSetUsage `json:"applied_sets"`
}

type salesLogResponse struct {
	Sales []saleLog `json:"sales"`
}

type saleLog struct {
	SaleID            int64  `json:"sale_id"`
	CreatedAt         string `json:"created_at"`
	Total             int    `json:"total"`
	ItemSummary       string `json:"item_summary"`
	AppliedSets       string `json:"applied_sets"`
	AdjustmentID      *int64 `json:"adjustment_id,omitempty"`
	AdjustmentType    string `json:"adjustment_type,omitempty"`
	AdjustmentStatus  string `json:"adjustment_status,omitempty"`
	ReplacementSaleID *int64 `json:"replacement_sale_id,omitempty"`
}

type adjustmentRequest struct {
	Type   string `json:"type"`
	Reason string `json:"reason"`
}

type adjustmentResponse struct {
	AdjustmentID     int64          `json:"adjustment_id"`
	OriginalSaleID   int64          `json:"original_sale_id"`
	AdjustmentSaleID int64          `json:"adjustment_sale_id"`
	Type             string         `json:"type"`
	DraftItems       []checkoutItem `json:"draft_items,omitempty"`
}

type alreadyAdjustedError struct{}

func (e *alreadyAdjustedError) Error() string {
	return "この会計はすでに取消・返品・訂正済みです"
}

type invalidCorrectionError struct{}

func (e *invalidCorrectionError) Error() string {
	return "この訂正会計はすでに完了済みか、利用できません"
}

type saleLine struct {
	ProductID string `json:"product_id"`
	Name      string `json:"name"`
	UnitPrice int    `json:"unit_price"`
	Quantity  int    `json:"quantity"`
	Subtotal  int    `json:"subtotal"`
	Discount  int    `json:"discount"`
	Total     int    `json:"total"`
}

type appliedSetUsage struct {
	SetID    string `json:"set_id"`
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
	SetPrice int    `json:"set_price"`
	Discount int    `json:"discount"`
}

type calculatedSale struct {
	Lines       []saleLine
	AppliedSets []appliedSetUsage
	Subtotal    int
	Discount    int
	Total       int
}

func main() {
	config, err := parseRunConfig(os.Args[1:])
	if err != nil {
		if err.Error() != "" {
			fmt.Fprintln(os.Stderr, err)
		}
		fmt.Fprint(os.Stderr, usageText)
		return
	}
	if err := run(config); err != nil {
		log.Printf("POSを起動できません: %v", err)
	}
}

func parseRunConfig(arguments []string) (runConfig, error) {
	if len(arguments) != 1 {
		return runConfig{}, errors.New("起動モードを1つ指定してください")
	}
	switch arguments[0] {
	case "--deploy":
		return runConfig{Mode: "本番", DatabaseFile: envOr("DATABASE_FILE", defaultDatabaseFile), ExportCatalogOnShutdown: true, SelectDatabase: true}, nil
	case "--debug":
		return runConfig{Mode: "デバッグ", DatabaseFile: envOr("DEBUG_DATABASE_FILE", debugDatabaseFile), ResetOnShutdown: true}, nil
	case "--test":
		return runConfig{Mode: "テスト", DatabaseFile: envOr("TEST_DATABASE_FILE", testDatabaseFile), ResetOnShutdown: true}, nil
	case "--help", "-h":
		return runConfig{}, errors.New("")
	default:
		return runConfig{}, fmt.Errorf("不明な起動モードです: %s", arguments[0])
	}
}

func run(config runConfig) error {
	productsFile := envOr("PRODUCTS_FILE", defaultProductsFile)
	if config.ResetOnShutdown {
		if err := resetSQLiteDatabase(config.DatabaseFile); err != nil {
			return fmt.Errorf("テスト用データベースを初期化できません: %w", err)
		}
	}
	if config.ResetOnShutdown {
		defer func() {
			if err := resetSQLiteDatabase(config.DatabaseFile); err != nil {
				log.Printf("テスト用データベースの初期化に失敗しました: %v", err)
				return
			}
			log.Printf("%sモードのデータベースを初期化しました", config.Mode)
		}()
	}
	manager := &eventManager{
		config:           config,
		productsTemplate: productsFile,
		eventsDirectory:  envOr("EVENTS_DIR", defaultEventsDir),
	}
	if !config.SelectDatabase {
		if err := manager.open(eventRecord{ID: "current", DatabaseFile: config.DatabaseFile, ProductsFile: productsFile}); err != nil {
			return err
		}
	}
	defer func() {
		if err := manager.close(); err != nil {
			log.Printf("POSデータベースの終了処理に失敗しました: %v", err)
		}
	}()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/events/selection", manager.handleEventSelection)
	mux.HandleFunc("/api/events", manager.handleEvents)
	mux.HandleFunc("/api/events/create", manager.handleCreateEvent)
	mux.HandleFunc("/api/events/open", manager.handleOpenEvent)
	mux.HandleFunc("/api/products", manager.withApp((*App).handleProducts))
	mux.HandleFunc("/api/inventory", manager.withApp((*App).handleInventory))
	mux.HandleFunc("/api/inventory/", manager.withApp((*App).handleInventoryItem))
	mux.HandleFunc("/api/checkout", manager.withApp((*App).handleCheckout))
	mux.HandleFunc("/api/sales", manager.withApp((*App).handleSales))
	mux.HandleFunc("/api/sales/export.csv", manager.withApp((*App).handleSalesCSV))
	mux.HandleFunc("/api/sales/", manager.withApp((*App).handleSaleAction))
	mux.HandleFunc("/api/settings/catalog", manager.withApp((*App).handleSettingsCatalog))
	mux.HandleFunc("/api/settings/products/", manager.withApp((*App).handleSettingsProduct))
	mux.HandleFunc("/api/settings/set-discounts", manager.withApp((*App).handleSettingsSetDiscounts))
	mux.HandleFunc("/api/settings/audit", manager.withApp((*App).handleSettingsAudit))
	mux.HandleFunc("/api/audit/export", manager.withApp((*App).handleAuditExport))
	mux.HandleFunc("/api/audit/operations/export", manager.withApp((*App).handleOperationsAuditExport))
	mux.Handle("/", http.FileServer(http.Dir("web")))

	port := envOr("PORT", "8080")
	if _, err := parsePort(port); err != nil {
		return err
	}
	server := &http.Server{
		Addr:              ":" + port,
		Handler:           loggingMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	serverErrors := make(chan error, 1)
	go func() {
		serverErrors <- server.ListenAndServe()
	}()
	log.Printf("%sモードでPOSを http://localhost:%s に起動しました", config.Mode, port)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)
	select {
	case <-stop:
		log.Println("POSを停止します")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("サーバー停止時のエラー: %w", err)
		}
		return nil
	case err := <-serverErrors:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("サーバーの起動に失敗しました: %w", err)
	}
}

func resetSQLiteDatabase(path string) error {
	if path == ":memory:" {
		return nil
	}
	var cleanupErrors []error
	for _, candidate := range []string{path, path + "-shm", path + "-wal"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErrors = append(cleanupErrors, fmt.Errorf("%s: %w", candidate, err))
		}
	}
	return errors.Join(cleanupErrors...)
}

func envOr(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func (manager *eventManager) currentApp() *App {
	manager.mu.RLock()
	defer manager.mu.RUnlock()
	return manager.app
}

func (manager *eventManager) withApp(handler func(*App, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		app := manager.currentApp()
		if app == nil {
			writeError(w, http.StatusConflict, "先に使用するイベントDBを選択してください")
			return
		}
		handler(app, w, r)
	}
}

func (manager *eventManager) handleEventSelection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, eventSelectionResponse{Selected: manager.currentApp() != nil})
}

func (manager *eventManager) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	events, err := manager.listEvents()
	if err != nil {
		log.Printf("イベントDB一覧の取得に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "イベントDB一覧を取得できませんでした")
		return
	}
	writeJSON(w, http.StatusOK, eventListResponse{Events: events})
}

func (manager *eventManager) handleCreateEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !manager.config.SelectDatabase {
		writeError(w, http.StatusConflict, "このモードではイベントDBを作成できません")
		return
	}
	request, err := readEventRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "イベントDB名を読み取れません")
		return
	}
	record, err := manager.create(request.ID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, record)
}

func (manager *eventManager) handleOpenEvent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	if !manager.config.SelectDatabase {
		writeError(w, http.StatusConflict, "このモードではイベントDBを選択できません")
		return
	}
	request, err := readEventRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "イベントDB名を読み取れません")
		return
	}
	record, err := manager.findEvent(request.ID)
	if err != nil {
		writeError(w, http.StatusNotFound, "イベントDBが見つかりません")
		return
	}
	if err := manager.open(record); err != nil {
		log.Printf("イベントDBを開けません: %v", err)
		writeError(w, http.StatusBadRequest, "イベントDBを開けませんでした")
		return
	}
	writeJSON(w, http.StatusOK, record)
}

func readEventRequest(w http.ResponseWriter, r *http.Request) (eventRequest, error) {
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request eventRequest
	if err := decoder.Decode(&request); err != nil {
		return eventRequest{}, err
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		return eventRequest{}, err
	}
	request.ID = strings.TrimSpace(request.ID)
	return request, nil
}

func (manager *eventManager) listEvents() ([]eventRecord, error) {
	events := make([]eventRecord, 0)
	if _, err := os.Stat(manager.config.DatabaseFile); err == nil {
		if _, err := os.Stat(manager.productsTemplate); err == nil {
			events = append(events, eventRecord{ID: "default", DatabaseFile: manager.config.DatabaseFile, ProductsFile: manager.productsTemplate, PersistCatalog: false})
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	entries, err := os.ReadDir(manager.eventsDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return events, nil
	}
	if err != nil {
		return nil, err
	}
	seenIDs := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		record := eventRecord{
			ID:             id,
			DatabaseFile:   filepath.Join(manager.eventsDirectory, id, "pos.db"),
			ProductsFile:   filepath.Join(manager.eventsDirectory, id, "products.yaml"),
			PersistCatalog: true,
		}
		if _, err := os.Stat(record.DatabaseFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		if _, err := os.Stat(record.ProductsFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		events = append(events, record)
		seenIDs[id] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".db" {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".db")
		if seenIDs[id] {
			continue
		}
		productsFile := filepath.Join(manager.eventsDirectory, id+".yaml")
		if _, err := os.Stat(productsFile); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		events = append(events, eventRecord{
			ID:             id,
			DatabaseFile:   filepath.Join(manager.eventsDirectory, entry.Name()),
			ProductsFile:   productsFile,
			PersistCatalog: true,
		})
	}
	sort.Slice(events, func(left, right int) bool { return events[left].ID < events[right].ID })
	return events, nil
}

func (manager *eventManager) findEvent(id string) (eventRecord, error) {
	events, err := manager.listEvents()
	if err != nil {
		return eventRecord{}, err
	}
	for _, event := range events {
		if event.ID == id {
			return event, nil
		}
	}
	return eventRecord{}, os.ErrNotExist
}

func (manager *eventManager) create(id string) (eventRecord, error) {
	if id == "default" || !catalogIDPattern.MatchString(id) {
		return eventRecord{}, errors.New("DB名は英数字・ハイフン・アンダースコアを1〜64文字で指定してください")
	}
	manager.mu.RLock()
	alreadySelected := manager.app != nil
	manager.mu.RUnlock()
	if alreadySelected {
		return eventRecord{}, errors.New("すでにイベントDBが選択されています")
	}
	template, err := loadCatalog(manager.productsTemplate)
	if err != nil {
		return eventRecord{}, fmt.Errorf("新規作成用の商品YAMLを読み込めません: %w", err)
	}
	record := eventRecord{
		ID:             id,
		DatabaseFile:   filepath.Join(manager.eventsDirectory, id, "pos.db"),
		ProductsFile:   filepath.Join(manager.eventsDirectory, id, "products.yaml"),
		PersistCatalog: true,
	}
	eventDirectory := filepath.Dir(record.DatabaseFile)
	if _, err := os.Stat(eventDirectory); err == nil {
		return eventRecord{}, errors.New("同じDB名のイベントがすでに存在します")
	} else if !errors.Is(err, os.ErrNotExist) {
		return eventRecord{}, err
	}
	for _, path := range []string{filepath.Join(manager.eventsDirectory, id+".db"), filepath.Join(manager.eventsDirectory, id+".yaml")} {
		if _, err := os.Stat(path); err == nil {
			return eventRecord{}, errors.New("同じDB名のイベントがすでに存在します")
		} else if !errors.Is(err, os.ErrNotExist) {
			return eventRecord{}, err
		}
	}
	if err := os.MkdirAll(eventDirectory, 0o755); err != nil {
		return eventRecord{}, err
	}
	blankCatalog := ProductCatalog{
		Store:        template.Store,
		Products:     []Product{},
		SetDiscounts: []SetDiscount{},
		Audit:        AuditSettings{},
	}
	contents, err := yaml.Marshal(blankCatalog)
	if err != nil {
		return eventRecord{}, err
	}
	if err := os.WriteFile(record.ProductsFile, contents, 0o644); err != nil {
		return eventRecord{}, err
	}
	if err := manager.open(record); err != nil {
		return eventRecord{}, err
	}
	return record, nil
}

func (manager *eventManager) open(record eventRecord) error {
	manager.mu.Lock()
	defer manager.mu.Unlock()
	if manager.app != nil {
		return errors.New("すでにイベントDBが選択されています")
	}
	catalog, err := loadCatalog(record.ProductsFile)
	if err != nil {
		return fmt.Errorf("商品YAMLを読み込めません: %w", err)
	}
	db, err := openDatabase(record.DatabaseFile)
	if err != nil {
		return fmt.Errorf("SQLiteを初期化できません: %w", err)
	}
	if err := syncInventory(db, catalog.Products); err != nil {
		db.Close()
		return fmt.Errorf("在庫を初期化できません: %w", err)
	}
	manager.app = &App{catalog: catalog, byID: productMap(catalog.Products), db: db, eventName: record.ID}
	manager.selected = record
	return nil
}

func (manager *eventManager) close() error {
	manager.mu.Lock()
	app := manager.app
	selected := manager.selected
	manager.app = nil
	manager.selected = eventRecord{}
	manager.mu.Unlock()
	if app == nil {
		return nil
	}
	var closeErrors []error
	if manager.config.ExportCatalogOnShutdown && selected.PersistCatalog {
		if err := app.exportCatalog(selected.ProductsFile); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("商品YAMLの書き出し: %w", err))
		} else {
			log.Printf("商品・在庫設定を %s に書き出しました", selected.ProductsFile)
		}
	}
	if err := app.db.Close(); err != nil {
		closeErrors = append(closeErrors, err)
	}
	return errors.Join(closeErrors...)
}

func loadCatalog(path string) (ProductCatalog, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ProductCatalog{}, err
	}
	var catalog ProductCatalog
	if err := yaml.Unmarshal(contents, &catalog); err != nil {
		return ProductCatalog{}, fmt.Errorf("YAML形式が不正です: %w", err)
	}
	if err := validateCatalog(catalog); err != nil {
		return ProductCatalog{}, err
	}
	if catalog.Store.Currency == "" {
		catalog.Store.Currency = "JPY"
	}
	return catalog, nil
}

func validateCatalog(catalog ProductCatalog) error {
	productsByID := make(map[string]Product, len(catalog.Products))
	for _, product := range catalog.Products {
		if strings.TrimSpace(product.ID) == "" || strings.TrimSpace(product.Name) == "" {
			return errors.New("各商品にidとnameが必要です")
		}
		if _, exists := productsByID[product.ID]; exists {
			return fmt.Errorf("商品idが重複しています: %s", product.ID)
		}
		productsByID[product.ID] = product
		if product.UnitPrice < 0 {
			return fmt.Errorf("単価は0以上にしてください: %s", product.ID)
		}
		if product.InitialStock < 0 {
			return fmt.Errorf("初期在庫は0以上にしてください: %s", product.ID)
		}
		if product.DeprecatedSetDiscount != nil {
			return fmt.Errorf("%s の set_discount は廃止されました。set_discounts に移動してください", product.ID)
		}
	}
	seenDiscounts := make(map[string]bool, len(catalog.SetDiscounts))
	for _, discount := range catalog.SetDiscounts {
		if strings.TrimSpace(discount.ID) == "" || strings.TrimSpace(discount.Name) == "" {
			return errors.New("各セット割引にidとnameが必要です")
		}
		if seenDiscounts[discount.ID] {
			return fmt.Errorf("セット割引idが重複しています: %s", discount.ID)
		}
		seenDiscounts[discount.ID] = true
		if len(discount.Items) == 0 || discount.SetPrice < 0 {
			return fmt.Errorf("セット割引が不正です: %s", discount.ID)
		}
		itemsInSet := make(map[string]bool, len(discount.Items))
		regularPrice := 0
		for _, item := range discount.Items {
			product, exists := productsByID[item.ProductID]
			if !exists || item.Quantity <= 0 || itemsInSet[item.ProductID] {
				return fmt.Errorf("セット割引 %s の対象商品または数量が不正です", discount.ID)
			}
			itemsInSet[item.ProductID] = true
			regularPrice += product.UnitPrice * item.Quantity
		}
		if discount.SetPrice >= regularPrice {
			return fmt.Errorf("セット割引 %s のセット価格は通常価格より低くしてください", discount.ID)
		}
	}
	return nil
}

func productMap(products []Product) map[string]Product {
	byID := make(map[string]Product, len(products))
	for _, product := range products {
		byID[product.ID] = product
	}
	return byID
}

func cloneCatalog(catalog ProductCatalog) ProductCatalog {
	clone := catalog
	clone.Products = make([]Product, len(catalog.Products))
	copy(clone.Products, catalog.Products)
	clone.SetDiscounts = make([]SetDiscount, len(catalog.SetDiscounts))
	for index, discount := range catalog.SetDiscounts {
		clone.SetDiscounts[index] = discount
		clone.SetDiscounts[index].Items = append([]SetDiscountItem(nil), discount.Items...)
	}
	return clone
}

func (app *App) catalogSnapshot() ProductCatalog {
	app.catalogMu.RLock()
	defer app.catalogMu.RUnlock()
	return cloneCatalog(app.catalog)
}

func (app *App) productByID(productID string) (Product, bool) {
	app.catalogMu.RLock()
	defer app.catalogMu.RUnlock()
	product, exists := app.byID[productID]
	return product, exists
}

func (app *App) catalogWithCurrentStock() (ProductCatalog, error) {
	catalog := app.catalogSnapshot()
	for index := range catalog.Products {
		if err := app.db.QueryRow(`SELECT quantity FROM inventory WHERE product_id = ?`, catalog.Products[index].ID).Scan(&catalog.Products[index].InitialStock); err != nil {
			return ProductCatalog{}, fmt.Errorf("%s の在庫を取得できません: %w", catalog.Products[index].ID, err)
		}
	}
	return catalog, nil
}

func (app *App) exportCatalog(path string) error {
	catalog, err := app.catalogWithCurrentStock()
	if err != nil {
		return err
	}
	contents, err := yaml.Marshal(catalog)
	if err != nil {
		return err
	}
	temporaryPath := path + ".tmp"
	defer os.Remove(temporaryPath)
	if err := os.WriteFile(temporaryPath, contents, 0o644); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func openDatabase(path string) (*sql.DB, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA foreign_keys = ON;`); err != nil {
		db.Close()
		return nil, err
	}
	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS sales (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			subtotal INTEGER NOT NULL,
			discount INTEGER NOT NULL,
			total INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sale_items (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sale_id INTEGER NOT NULL REFERENCES sales(id),
			product_id TEXT NOT NULL,
			product_name TEXT NOT NULL,
			unit_price INTEGER NOT NULL,
			quantity INTEGER NOT NULL,
			subtotal INTEGER NOT NULL,
			discount INTEGER NOT NULL,
			total INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sale_set_discounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			sale_id INTEGER NOT NULL REFERENCES sales(id),
			set_id TEXT NOT NULL,
			set_name TEXT NOT NULL,
			quantity INTEGER NOT NULL,
			set_price INTEGER NOT NULL,
			discount INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sale_adjustments (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			original_sale_id INTEGER NOT NULL UNIQUE REFERENCES sales(id),
			adjustment_sale_id INTEGER NOT NULL UNIQUE REFERENCES sales(id),
			adjustment_type TEXT NOT NULL CHECK (adjustment_type IN ('void', 'refund', 'correction')),
			reason TEXT NOT NULL,
			status TEXT NOT NULL CHECK (status IN ('completed', 'pending')),
			replacement_sale_id INTEGER REFERENCES sales(id),
			created_at TEXT NOT NULL,
			completed_at TEXT
		);
		CREATE TABLE IF NOT EXISTS inventory (
			product_id TEXT PRIMARY KEY,
			quantity INTEGER NOT NULL CHECK (quantity >= 0),
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS inventory_movements (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			product_id TEXT NOT NULL,
			delta INTEGER NOT NULL,
			reason TEXT NOT NULL,
			created_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS operation_audit (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			category TEXT NOT NULL,
			action TEXT NOT NULL,
			subject_type TEXT NOT NULL,
			subject_id TEXT NOT NULL,
			details TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sale_items_sale_id ON sale_items(sale_id);
		CREATE INDEX IF NOT EXISTS idx_sale_set_discounts_sale_id ON sale_set_discounts(sale_id);
		CREATE INDEX IF NOT EXISTS idx_sale_adjustments_replacement_sale_id ON sale_adjustments(replacement_sale_id);
		CREATE INDEX IF NOT EXISTS idx_operation_audit_created_at ON operation_audit(created_at);
	`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`PRAGMA optimize`); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// syncInventory creates stock records for products that are newly added to the YAML.
// Existing records are intentionally kept so changing initial_stock cannot overwrite live stock.
func syncInventory(db *sql.DB, products []Product) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.Prepare(`
		INSERT OR IGNORE INTO inventory (product_id, quantity, updated_at) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer statement.Close()
	movementStatement, err := tx.Prepare(`
		INSERT INTO inventory_movements (product_id, delta, reason, created_at) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer movementStatement.Close()
	now := time.Now().UTC().Format(time.RFC3339)
	for _, product := range products {
		result, err := statement.Exec(product.ID, product.InitialStock, now)
		if err != nil {
			return err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if rowsAffected == 1 {
			if _, err := movementStatement.Exec(product.ID, product.InitialStock, "initial", now); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func recordOperation(tx *sql.Tx, entry operationAuditEntry) error {
	details, err := json.Marshal(entry.Details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`
		INSERT INTO operation_audit (created_at, category, action, subject_type, subject_id, details)
		VALUES (?, ?, ?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339), entry.Category, entry.Action, entry.SubjectType, entry.SubjectID, string(details),
	)
	return err
}

func (app *App) recordOperation(entry operationAuditEntry) error {
	tx, err := app.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := recordOperation(tx, entry); err != nil {
		return err
	}
	return tx.Commit()
}

func (app *App) handleProducts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, app.catalogSnapshot())
}

func (app *App) handleInventory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	rows, err := app.db.Query(`SELECT product_id, quantity FROM inventory`)
	if err != nil {
		log.Printf("在庫の取得に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "在庫を取得できませんでした")
		return
	}
	defer rows.Close()
	catalog := app.catalogSnapshot()
	quantities := make(map[string]int, len(catalog.Products))
	for rows.Next() {
		var productID string
		var quantity int
		if err := rows.Scan(&productID, &quantity); err != nil {
			log.Printf("在庫の読み取りに失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "在庫を取得できませんでした")
			return
		}
		quantities[productID] = quantity
	}
	if err := rows.Err(); err != nil {
		log.Printf("在庫の読み取りに失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "在庫を取得できませんでした")
		return
	}
	items := make([]inventoryItem, 0, len(catalog.Products))
	for _, product := range catalog.Products {
		items = append(items, inventoryItem{ProductID: product.ID, Name: product.Name, Quantity: quantities[product.ID]})
	}
	writeJSON(w, http.StatusOK, inventoryResponse{Items: items})
}

func (app *App) handleInventoryItem(w http.ResponseWriter, r *http.Request) {
	productID := strings.TrimPrefix(r.URL.Path, "/api/inventory/")
	if productID == "" || strings.Contains(productID, "/") {
		writeError(w, http.StatusNotFound, "商品が見つかりません")
		return
	}
	product, exists := app.productByID(productID)
	if !exists {
		writeError(w, http.StatusNotFound, "商品が見つかりません")
		return
	}
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request inventoryUpdateRequest
	if err := decoder.Decode(&request); err != nil || ensureSingleJSONValue(decoder) != nil {
		writeError(w, http.StatusBadRequest, "在庫データを読み取れません")
		return
	}
	if request.Quantity < 0 {
		writeError(w, http.StatusBadRequest, "在庫数は0以上にしてください")
		return
	}

	tx, err := app.db.Begin()
	if err != nil {
		log.Printf("在庫更新の開始に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "在庫を更新できませんでした")
		return
	}
	defer tx.Rollback()
	var currentQuantity int
	if err := tx.QueryRow(`SELECT quantity FROM inventory WHERE product_id = ?`, productID).Scan(&currentQuantity); err != nil {
		log.Printf("在庫更新時の商品取得に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "在庫を更新できませんでした")
		return
	}
	if currentQuantity != request.Quantity {
		now := time.Now().UTC().Format(time.RFC3339)
		if _, err := tx.Exec(`UPDATE inventory SET quantity = ?, updated_at = ? WHERE product_id = ?`, request.Quantity, now, productID); err != nil {
			log.Printf("在庫更新に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "在庫を更新できませんでした")
			return
		}
		if _, err := tx.Exec(`
			INSERT INTO inventory_movements (product_id, delta, reason, created_at) VALUES (?, ?, ?, ?)`,
			productID, request.Quantity-currentQuantity, "adjustment", now,
		); err != nil {
			log.Printf("在庫履歴の保存に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "在庫を更新できませんでした")
			return
		}
		if err := recordOperation(tx, operationAuditEntry{
			Category: "inventory", Action: "inventory_adjustment", SubjectType: "product", SubjectID: productID,
			Details: map[string]any{"product_name": product.Name, "before_quantity": currentQuantity, "after_quantity": request.Quantity},
		}); err != nil {
			log.Printf("在庫操作の監査履歴保存に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "在庫を更新できませんでした")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("在庫更新の確定に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "在庫を更新できませんでした")
		return
	}
	writeJSON(w, http.StatusOK, inventoryItem{ProductID: product.ID, Name: product.Name, Quantity: request.Quantity})
}

func (app *App) handleSettingsCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	catalog, err := app.catalogWithCurrentStock()
	if err != nil {
		log.Printf("設定用の商品情報の取得に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "設定を読み込めませんでした")
		return
	}
	writeJSON(w, http.StatusOK, catalog)
}

func (app *App) handleSettingsProduct(w http.ResponseWriter, r *http.Request) {
	productID := strings.TrimPrefix(r.URL.Path, "/api/settings/products/")
	if productID == "" || strings.Contains(productID, "/") {
		writeError(w, http.StatusNotFound, "商品が見つかりません")
		return
	}
	switch r.Method {
	case http.MethodPut:
		app.saveCatalogProduct(w, r, productID)
	case http.MethodDelete:
		app.deleteCatalogProduct(w, productID)
	default:
		methodNotAllowed(w, "PUT, DELETE")
	}
}

func (app *App) saveCatalogProduct(w http.ResponseWriter, r *http.Request, productID string) {
	if !catalogIDPattern.MatchString(productID) {
		writeError(w, http.StatusBadRequest, "商品IDは英数字・ハイフン・アンダースコアを1〜64文字で指定してください")
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request catalogProductRequest
	if err := decoder.Decode(&request); err != nil || ensureSingleJSONValue(decoder) != nil {
		writeError(w, http.StatusBadRequest, "商品データを読み取れません")
		return
	}
	request.Name = strings.TrimSpace(request.Name)
	if request.Name == "" || request.UnitPrice < 0 || request.Stock < 0 {
		writeError(w, http.StatusBadRequest, "商品名、単価、在庫数を正しく入力してください")
		return
	}

	app.catalogMu.Lock()
	defer app.catalogMu.Unlock()
	candidate := cloneCatalog(app.catalog)
	product := Product{ID: productID, Name: request.Name, UnitPrice: request.UnitPrice, InitialStock: request.Stock}
	existing := false
	var previousProduct Product
	for index := range candidate.Products {
		if candidate.Products[index].ID == productID {
			previousProduct = candidate.Products[index]
			candidate.Products[index] = product
			existing = true
			break
		}
	}
	if !existing {
		candidate.Products = append(candidate.Products, product)
	}
	if err := validateCatalog(candidate); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	tx, err := app.db.Begin()
	if err != nil {
		log.Printf("商品設定の更新を開始できません: %v", err)
		writeError(w, http.StatusInternalServerError, "商品を保存できませんでした")
		return
	}
	defer tx.Rollback()
	var currentStock int
	stockRecordExists := true
	if err := tx.QueryRow(`SELECT quantity FROM inventory WHERE product_id = ?`, productID).Scan(&currentStock); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			stockRecordExists = false
			currentStock = 0
		} else {
			log.Printf("商品設定の在庫取得に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "商品を保存できませんでした")
			return
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`
		INSERT INTO inventory (product_id, quantity, updated_at) VALUES (?, ?, ?)
		ON CONFLICT(product_id) DO UPDATE SET quantity = excluded.quantity, updated_at = excluded.updated_at`,
		productID, request.Stock, now,
	); err != nil {
		log.Printf("商品設定の在庫更新に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "商品を保存できませんでした")
		return
	}
	if !stockRecordExists || request.Stock != currentStock {
		reason := "catalog_update"
		if !existing {
			reason = "catalog_add"
		}
		if _, err := tx.Exec(`
			INSERT INTO inventory_movements (product_id, delta, reason, created_at) VALUES (?, ?, ?, ?)`,
			productID, request.Stock-currentStock, reason, now,
		); err != nil {
			log.Printf("商品設定の在庫履歴保存に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "商品を保存できませんでした")
			return
		}
	}
	productChanged := !existing || previousProduct.Name != product.Name || previousProduct.UnitPrice != product.UnitPrice || request.Stock != currentStock
	if productChanged {
		action := "product_update"
		if !existing {
			action = "product_create"
		}
		details := map[string]any{"after": product, "before_stock": currentStock, "after_stock": request.Stock}
		if existing {
			details["before"] = previousProduct
		}
		if err := recordOperation(tx, operationAuditEntry{
			Category: "settings", Action: action, SubjectType: "product", SubjectID: productID, Details: details,
		}); err != nil {
			log.Printf("商品設定の監査履歴保存に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "商品を保存できませんでした")
			return
		}
	}
	if err := tx.Commit(); err != nil {
		log.Printf("商品設定の更新を確定できません: %v", err)
		writeError(w, http.StatusInternalServerError, "商品を保存できませんでした")
		return
	}
	app.catalog = candidate
	app.byID = productMap(candidate.Products)
	writeJSON(w, http.StatusOK, product)
}

func (app *App) deleteCatalogProduct(w http.ResponseWriter, productID string) {
	app.catalogMu.Lock()
	defer app.catalogMu.Unlock()
	candidate := cloneCatalog(app.catalog)
	productIndex := -1
	for index, product := range candidate.Products {
		if product.ID == productID {
			productIndex = index
			break
		}
	}
	if productIndex == -1 {
		writeError(w, http.StatusNotFound, "商品が見つかりません")
		return
	}
	if len(candidate.Products) == 1 {
		writeError(w, http.StatusBadRequest, "最後の1商品は削除できません")
		return
	}
	deletedProduct := candidate.Products[productIndex]
	candidate.Products = append(candidate.Products[:productIndex], candidate.Products[productIndex+1:]...)
	filteredDiscounts := make([]SetDiscount, 0, len(candidate.SetDiscounts))
	removedSetIDs := make([]string, 0)
	for _, discount := range candidate.SetDiscounts {
		usesProduct := false
		for _, item := range discount.Items {
			if item.ProductID == productID {
				usesProduct = true
				break
			}
		}
		if usesProduct {
			removedSetIDs = append(removedSetIDs, discount.ID)
		}
		if !usesProduct {
			filteredDiscounts = append(filteredDiscounts, discount)
		}
	}
	candidate.SetDiscounts = filteredDiscounts
	if err := app.recordOperation(operationAuditEntry{
		Category: "settings", Action: "product_delete", SubjectType: "product", SubjectID: productID,
		Details: map[string]any{"product": deletedProduct, "removed_set_ids": removedSetIDs},
	}); err != nil {
		log.Printf("商品削除の監査履歴保存に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "商品を削除できませんでした")
		return
	}
	app.catalog = candidate
	app.byID = productMap(candidate.Products)
	writeJSON(w, http.StatusOK, map[string]string{"message": "商品を削除しました"})
}

func (app *App) handleSettingsSetDiscounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request setDiscountsRequest
	if err := decoder.Decode(&request); err != nil || ensureSingleJSONValue(decoder) != nil {
		writeError(w, http.StatusBadRequest, "セット割引データを読み取れません")
		return
	}
	app.catalogMu.Lock()
	defer app.catalogMu.Unlock()
	candidate := cloneCatalog(app.catalog)
	previousSetDiscounts := candidate.SetDiscounts
	candidate.SetDiscounts = request.SetDiscounts
	if err := validateCatalog(candidate); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := app.recordOperation(operationAuditEntry{
		Category: "settings", Action: "set_discounts_replace", SubjectType: "set_discounts", SubjectID: "all",
		Details: map[string]any{"before": previousSetDiscounts, "after": request.SetDiscounts},
	}); err != nil {
		log.Printf("セット割引設定の監査履歴保存に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "セット割引を保存できませんでした")
		return
	}
	app.catalog = candidate
	writeJSON(w, http.StatusOK, candidate.SetDiscounts)
}

func (app *App) handleSettingsAudit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		methodNotAllowed(w, http.MethodPut)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request auditSettingsRequest
	if err := decoder.Decode(&request); err != nil || ensureSingleJSONValue(decoder) != nil {
		writeError(w, http.StatusBadRequest, "監査出力設定を読み取れません")
		return
	}
	app.catalogMu.Lock()
	defer app.catalogMu.Unlock()
	previousDirectory := app.catalog.Audit.OutputDirectory
	newDirectory := strings.TrimSpace(request.OutputDirectory)
	if previousDirectory != newDirectory {
		if err := app.recordOperation(operationAuditEntry{
			Category: "settings", Action: "audit_output_directory_update", SubjectType: "audit", SubjectID: "output_directory",
			Details: map[string]any{"before": previousDirectory, "after": newDirectory},
		}); err != nil {
			log.Printf("監査出力先設定の監査履歴保存に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "監査出力先を保存できませんでした")
			return
		}
	}
	app.catalog.Audit.OutputDirectory = newDirectory
	response := app.catalog.Audit
	writeJSON(w, http.StatusOK, response)
}

func (app *App) handleAuditExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	directory := strings.TrimSpace(app.catalogSnapshot().Audit.OutputDirectory)
	if directory == "" {
		writeError(w, http.StatusBadRequest, "先に監査CSVの出力先ディレクトリを設定してください")
		return
	}
	absDirectory, err := filepath.Abs(directory)
	if err != nil {
		writeError(w, http.StatusBadRequest, "出力先ディレクトリが不正です")
		return
	}
	if err := os.MkdirAll(absDirectory, 0o755); err != nil {
		log.Printf("監査CSVの出力先を作成できません: %v", err)
		writeError(w, http.StatusInternalServerError, "監査CSVの出力先を作成できませんでした")
		return
	}
	path := filepath.Join(absDirectory, "pos-audit-"+time.Now().Format("20060102-150405.000000000")+".csv")
	file, err := os.Create(path)
	if err != nil {
		log.Printf("監査CSVを作成できません: %v", err)
		writeError(w, http.StatusInternalServerError, "監査CSVを出力できませんでした")
		return
	}
	if err := app.writeSalesCSV(file); err != nil {
		file.Close()
		log.Printf("監査CSVの書き込みに失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "監査CSVを出力できませんでした")
		return
	}
	if err := file.Close(); err != nil {
		log.Printf("監査CSVを閉じられません: %v", err)
		writeError(w, http.StatusInternalServerError, "監査CSVを出力できませんでした")
		return
	}
	writeJSON(w, http.StatusCreated, auditExportResponse{Path: path})
}

func (app *App) handleOperationsAuditExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	directory := strings.TrimSpace(app.catalogSnapshot().Audit.OutputDirectory)
	if directory == "" {
		writeError(w, http.StatusBadRequest, "先に監査CSVの出力先ディレクトリを設定してください")
		return
	}
	absDirectory, err := filepath.Abs(directory)
	if err != nil {
		writeError(w, http.StatusBadRequest, "出力先ディレクトリが不正です")
		return
	}
	if err := os.MkdirAll(absDirectory, 0o755); err != nil {
		log.Printf("運用監査CSVの出力先を作成できません: %v", err)
		writeError(w, http.StatusInternalServerError, "運用監査CSVの出力先を作成できませんでした")
		return
	}
	path := filepath.Join(absDirectory, "pos-operations-audit-"+time.Now().Format("20060102-150405.000000000")+".csv")
	file, err := os.Create(path)
	if err != nil {
		log.Printf("運用監査CSVを作成できません: %v", err)
		writeError(w, http.StatusInternalServerError, "運用監査CSVを出力できませんでした")
		return
	}
	if err := app.writeOperationsCSV(file); err != nil {
		file.Close()
		log.Printf("運用監査CSVの書き込みに失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "運用監査CSVを出力できませんでした")
		return
	}
	if err := file.Close(); err != nil {
		log.Printf("運用監査CSVを閉じられません: %v", err)
		writeError(w, http.StatusInternalServerError, "運用監査CSVを出力できませんでした")
		return
	}
	writeJSON(w, http.StatusCreated, auditExportResponse{Path: path})
}

func (app *App) handleCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		methodNotAllowed(w, http.MethodPost)
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request checkoutRequest
	if err := decoder.Decode(&request); err != nil {
		writeError(w, http.StatusBadRequest, "会計データを読み取れません")
		return
	}
	if err := ensureSingleJSONValue(decoder); err != nil {
		writeError(w, http.StatusBadRequest, "会計データの形式が不正です")
		return
	}

	sale, err := calculateSale(app.catalogSnapshot(), request.Items)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	saleID, err := app.saveSaleWithCorrection(sale, request.CorrectionAdjustmentID)
	if err != nil {
		var stockError *insufficientStockError
		if errors.As(err, &stockError) {
			writeError(w, http.StatusConflict, stockError.Error())
			return
		}
		var correctionError *invalidCorrectionError
		if errors.As(err, &correctionError) {
			writeError(w, http.StatusConflict, correctionError.Error())
			return
		}
		log.Printf("会計の保存に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "会計を保存できませんでした")
		return
	}
	writeJSON(w, http.StatusCreated, checkoutResponse{
		SaleID: saleID, Subtotal: sale.Subtotal, Discount: sale.Discount, Total: sale.Total,
		Items: sale.Lines, AppliedSets: sale.AppliedSets,
	})
}

func (app *App) handleSaleAction(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/sales/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] != "adjustments" {
		writeError(w, http.StatusNotFound, "操作対象の会計が見つかりません")
		return
	}
	saleID, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || saleID < 1 {
		writeError(w, http.StatusNotFound, "操作対象の会計が見つかりません")
		return
	}
	if r.Method == http.MethodGet {
		response, err := app.pendingCorrectionDraft(saleID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, "再開できる訂正会計が見つかりません")
				return
			}
			log.Printf("訂正会計の取得に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "訂正会計を取得できませんでした")
			return
		}
		writeJSON(w, http.StatusOK, response)
		return
	}
	if r.Method != http.MethodPost {
		methodNotAllowed(w, "GET, POST")
		return
	}
	defer r.Body.Close()
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	var request adjustmentRequest
	if err := decoder.Decode(&request); err != nil || ensureSingleJSONValue(decoder) != nil {
		writeError(w, http.StatusBadRequest, "取消・訂正データを読み取れません")
		return
	}
	request.Type = strings.TrimSpace(request.Type)
	request.Reason = strings.TrimSpace(request.Reason)
	if !validAdjustmentType(request.Type) || request.Reason == "" || len([]rune(request.Reason)) > 200 {
		writeError(w, http.StatusBadRequest, "取消・訂正の種類または理由が不正です")
		return
	}

	response, err := app.createSaleAdjustment(saleID, request)
	if err != nil {
		var adjustedError *alreadyAdjustedError
		switch {
		case errors.As(err, &adjustedError):
			writeError(w, http.StatusConflict, adjustedError.Error())
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, "会計が見つかりません")
		default:
			log.Printf("取消・訂正の保存に失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "取消・訂正を保存できませんでした")
		}
		return
	}
	writeJSON(w, http.StatusCreated, response)
}

func (app *App) pendingCorrectionDraft(originalSaleID int64) (adjustmentResponse, error) {
	tx, err := app.db.Begin()
	if err != nil {
		return adjustmentResponse{}, err
	}
	defer tx.Rollback()
	response := adjustmentResponse{OriginalSaleID: originalSaleID, Type: "correction"}
	if err := tx.QueryRow(`
		SELECT id, adjustment_sale_id
		FROM sale_adjustments
		WHERE original_sale_id = ? AND adjustment_type = 'correction' AND status = 'pending'`,
		originalSaleID,
	).Scan(&response.AdjustmentID, &response.AdjustmentSaleID); err != nil {
		return adjustmentResponse{}, err
	}
	lines, err := loadSaleLines(tx, originalSaleID)
	if err != nil {
		return adjustmentResponse{}, err
	}
	response.DraftItems = make([]checkoutItem, 0, len(lines))
	for _, line := range lines {
		response.DraftItems = append(response.DraftItems, checkoutItem{ProductID: line.ProductID, Quantity: line.Quantity})
	}
	if err := tx.Commit(); err != nil {
		return adjustmentResponse{}, err
	}
	return response, nil
}

func validAdjustmentType(adjustmentType string) bool {
	return adjustmentType == "void" || adjustmentType == "refund" || adjustmentType == "correction"
}

func (app *App) createSaleAdjustment(originalSaleID int64, request adjustmentRequest) (adjustmentResponse, error) {
	tx, err := app.db.Begin()
	if err != nil {
		return adjustmentResponse{}, err
	}
	defer tx.Rollback()
	var subtotal, discount, total int
	if err := tx.QueryRow(`SELECT subtotal, discount, total FROM sales WHERE id = ?`, originalSaleID).Scan(&subtotal, &discount, &total); err != nil {
		return adjustmentResponse{}, err
	}
	var adjustmentID int64
	if err := tx.QueryRow(`SELECT id FROM sale_adjustments WHERE original_sale_id = ? OR adjustment_sale_id = ?`, originalSaleID, originalSaleID).Scan(&adjustmentID); err == nil {
		return adjustmentResponse{}, &alreadyAdjustedError{}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return adjustmentResponse{}, err
	}

	lines, err := loadSaleLines(tx, originalSaleID)
	if err != nil {
		return adjustmentResponse{}, err
	}
	appliedSets, err := loadAppliedSets(tx, originalSaleID)
	if err != nil {
		return adjustmentResponse{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, line := range lines {
		result, err := tx.Exec(`
			UPDATE inventory SET quantity = quantity + ?, updated_at = ? WHERE product_id = ?`,
			line.Quantity, now, line.ProductID,
		)
		if err != nil {
			return adjustmentResponse{}, err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil || rowsAffected != 1 {
			return adjustmentResponse{}, fmt.Errorf("在庫レコードを復元できません: %s", line.ProductID)
		}
		if _, err := tx.Exec(`
			INSERT INTO inventory_movements (product_id, delta, reason, created_at) VALUES (?, ?, ?, ?)`,
			line.ProductID, line.Quantity, request.Type, now,
		); err != nil {
			return adjustmentResponse{}, err
		}
	}
	result, err := tx.Exec(
		`INSERT INTO sales (created_at, subtotal, discount, total) VALUES (?, ?, ?, ?)`,
		now, -subtotal, -discount, -total,
	)
	if err != nil {
		return adjustmentResponse{}, err
	}
	reversalSaleID, err := result.LastInsertId()
	if err != nil {
		return adjustmentResponse{}, err
	}
	itemStatement, err := tx.Prepare(`
		INSERT INTO sale_items (sale_id, product_id, product_name, unit_price, quantity, subtotal, discount, total)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return adjustmentResponse{}, err
	}
	defer itemStatement.Close()
	for _, line := range lines {
		if _, err := itemStatement.Exec(
			reversalSaleID, line.ProductID, line.Name, line.UnitPrice, -line.Quantity, -line.Subtotal, -line.Discount, -line.Total,
		); err != nil {
			return adjustmentResponse{}, err
		}
	}
	setStatement, err := tx.Prepare(`
		INSERT INTO sale_set_discounts (sale_id, set_id, set_name, quantity, set_price, discount)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return adjustmentResponse{}, err
	}
	defer setStatement.Close()
	for _, appliedSet := range appliedSets {
		if _, err := setStatement.Exec(
			reversalSaleID, appliedSet.SetID, appliedSet.Name, -appliedSet.Quantity, appliedSet.SetPrice, -appliedSet.Discount,
		); err != nil {
			return adjustmentResponse{}, err
		}
	}
	status := "completed"
	if request.Type == "correction" {
		status = "pending"
	}
	result, err = tx.Exec(`
		INSERT INTO sale_adjustments
		(original_sale_id, adjustment_sale_id, adjustment_type, reason, status, created_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		originalSaleID, reversalSaleID, request.Type, request.Reason, status, now,
	)
	if err != nil {
		return adjustmentResponse{}, err
	}
	adjustmentID, err = result.LastInsertId()
	if err != nil {
		return adjustmentResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return adjustmentResponse{}, err
	}
	response := adjustmentResponse{
		AdjustmentID: adjustmentID, OriginalSaleID: originalSaleID, AdjustmentSaleID: reversalSaleID, Type: request.Type,
	}
	if request.Type == "correction" {
		response.DraftItems = make([]checkoutItem, 0, len(lines))
		for _, line := range lines {
			response.DraftItems = append(response.DraftItems, checkoutItem{ProductID: line.ProductID, Quantity: line.Quantity})
		}
	}
	return response, nil
}

func loadSaleLines(tx *sql.Tx, saleID int64) ([]saleLine, error) {
	rows, err := tx.Query(`
		SELECT product_id, product_name, unit_price, quantity, subtotal, discount, total
		FROM sale_items WHERE sale_id = ? ORDER BY id`, saleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	lines := make([]saleLine, 0)
	for rows.Next() {
		var line saleLine
		if err := rows.Scan(&line.ProductID, &line.Name, &line.UnitPrice, &line.Quantity, &line.Subtotal, &line.Discount, &line.Total); err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, sql.ErrNoRows
	}
	return lines, nil
}

func loadAppliedSets(tx *sql.Tx, saleID int64) ([]appliedSetUsage, error) {
	rows, err := tx.Query(`
		SELECT set_id, set_name, quantity, set_price, discount
		FROM sale_set_discounts WHERE sale_id = ? ORDER BY id`, saleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	appliedSets := make([]appliedSetUsage, 0)
	for rows.Next() {
		var appliedSet appliedSetUsage
		if err := rows.Scan(&appliedSet.SetID, &appliedSet.Name, &appliedSet.Quantity, &appliedSet.SetPrice, &appliedSet.Discount); err != nil {
			return nil, err
		}
		appliedSets = append(appliedSets, appliedSet)
	}
	return appliedSets, rows.Err()
}

func (app *App) handleSales(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	limit, err := salesLimit(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	rows, err := app.db.Query(`
		SELECT
			s.id,
			s.created_at,
			s.total,
			COALESCE((
				SELECT group_concat(product_name || ' × ' || quantity, ' / ')
				FROM sale_items
				WHERE sale_id = s.id
			), ''),
			COALESCE((
				SELECT group_concat(set_name || ' × ' || quantity, ' / ')
				FROM sale_set_discounts
				WHERE sale_id = s.id
			), ''),
			a.id,
			a.adjustment_type,
			a.status,
			a.replacement_sale_id
		FROM sales AS s
		LEFT JOIN sale_adjustments AS a ON a.original_sale_id = s.id
		WHERE NOT EXISTS (
			SELECT 1 FROM sale_adjustments AS reversal WHERE reversal.adjustment_sale_id = s.id
		)
		ORDER BY s.id DESC
		LIMIT ?`, limit)
	if err != nil {
		log.Printf("売上ログの取得に失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "売上ログを取得できませんでした")
		return
	}
	defer rows.Close()
	logs := make([]saleLog, 0, limit)
	for rows.Next() {
		var logEntry saleLog
		var adjustmentID, replacementSaleID sql.NullInt64
		var adjustmentType, adjustmentStatus sql.NullString
		if err := rows.Scan(
			&logEntry.SaleID, &logEntry.CreatedAt, &logEntry.Total, &logEntry.ItemSummary, &logEntry.AppliedSets,
			&adjustmentID, &adjustmentType, &adjustmentStatus, &replacementSaleID,
		); err != nil {
			log.Printf("売上ログの読み取りに失敗しました: %v", err)
			writeError(w, http.StatusInternalServerError, "売上ログを取得できませんでした")
			return
		}
		if adjustmentID.Valid {
			logEntry.AdjustmentID = &adjustmentID.Int64
			logEntry.AdjustmentType = adjustmentType.String
			logEntry.AdjustmentStatus = adjustmentStatus.String
		}
		if replacementSaleID.Valid {
			logEntry.ReplacementSaleID = &replacementSaleID.Int64
		}
		logs = append(logs, logEntry)
	}
	if err := rows.Err(); err != nil {
		log.Printf("売上ログの読み取りに失敗しました: %v", err)
		writeError(w, http.StatusInternalServerError, "売上ログを取得できませんでした")
		return
	}
	writeJSON(w, http.StatusOK, salesLogResponse{Sales: logs})
}

func (app *App) handleSalesCSV(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="sales-log.csv"`)
	if err := app.writeSalesCSV(w); err != nil {
		log.Printf("CSV出力に失敗しました: %v", err)
	}
}

func (app *App) writeSalesCSV(destination io.Writer) error {
	rows, err := app.db.Query(`
		SELECT
			s.id,
			s.created_at,
			i.product_id,
			i.product_name,
			i.unit_price,
			i.quantity,
			i.subtotal,
			s.discount,
			s.total,
			COALESCE((
				SELECT group_concat(set_name || ' × ' || quantity, ' / ')
				FROM sale_set_discounts
				WHERE sale_id = s.id
			), ''),
			COALESCE(reversal.adjustment_type, 'sale'),
			COALESCE(reversal.original_sale_id, 0),
			COALESCE(original.replacement_sale_id, 0),
			COALESCE(reversal.reason, original.reason, '')
		FROM sales AS s
		INNER JOIN sale_items AS i ON i.sale_id = s.id
		LEFT JOIN sale_adjustments AS original ON original.original_sale_id = s.id
		LEFT JOIN sale_adjustments AS reversal ON reversal.adjustment_sale_id = s.id
		ORDER BY s.id ASC, i.id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err := destination.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	writer := csv.NewWriter(destination)
	eventName := app.auditEventName()
	if err := writer.Write([]string{"イベント名", eventName}); err != nil {
		return err
	}
	if err := writer.Write([]string{"出力日時", formatLogTime(time.Now().UTC().Format(time.RFC3339))}); err != nil {
		return err
	}
	if err := writer.Write(nil); err != nil {
		return err
	}
	if err := writer.Write([]string{"会計ID", "日時", "商品ID", "商品名", "単価", "数量", "小計", "セット割引", "合計", "適用セット", "取引種別", "元会計ID", "訂正後会計ID", "変更理由"}); err != nil {
		return err
	}
	for rows.Next() {
		var saleID int64
		var createdAt, productID, productName, appliedSets string
		var unitPrice, quantity, subtotal, discount, total int
		var transactionType, adjustmentReason string
		var originalSaleID, replacementSaleID int64
		if err := rows.Scan(
			&saleID, &createdAt, &productID, &productName, &unitPrice, &quantity, &subtotal, &discount, &total, &appliedSets,
			&transactionType, &originalSaleID, &replacementSaleID, &adjustmentReason,
		); err != nil {
			return err
		}
		originalSaleIDText := ""
		if originalSaleID != 0 {
			originalSaleIDText = strconv.FormatInt(originalSaleID, 10)
		}
		replacementSaleIDText := ""
		if replacementSaleID != 0 {
			replacementSaleIDText = strconv.FormatInt(replacementSaleID, 10)
		}
		record := []string{
			strconv.FormatInt(saleID, 10), formatLogTime(createdAt), productID, productName,
			strconv.Itoa(unitPrice), strconv.Itoa(quantity), strconv.Itoa(subtotal), strconv.Itoa(discount), strconv.Itoa(total), appliedSets,
			transactionType, originalSaleIDText, replacementSaleIDText,
			adjustmentReason,
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	summaryItems, total, err := app.salesSummary()
	if err != nil {
		return err
	}
	if err := writer.Write(nil); err != nil {
		return err
	}
	if err := writer.Write([]string{"純売上集計（取消・返品を反映）"}); err != nil {
		return err
	}
	if err := writer.Write([]string{"商品ID", "商品名", "販売数"}); err != nil {
		return err
	}
	for _, item := range summaryItems {
		if err := writer.Write([]string{item.ProductID, item.Name, strconv.Itoa(item.Quantity)}); err != nil {
			return err
		}
	}
	if err := writer.Write([]string{"合計売上（円）", strconv.Itoa(total)}); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

func (app *App) writeOperationsCSV(destination io.Writer) error {
	rows, err := app.db.Query(`
		SELECT id, created_at, category, action, subject_type, subject_id, details
		FROM operation_audit
		ORDER BY id ASC`)
	if err != nil {
		return err
	}
	defer rows.Close()
	if _, err := destination.Write([]byte("\xEF\xBB\xBF")); err != nil {
		return err
	}
	writer := csv.NewWriter(destination)
	if err := writer.Write([]string{"イベント名", app.auditEventName()}); err != nil {
		return err
	}
	if err := writer.Write([]string{"出力日時", formatLogTime(time.Now().UTC().Format(time.RFC3339))}); err != nil {
		return err
	}
	if err := writer.Write(nil); err != nil {
		return err
	}
	if err := writer.Write([]string{"記録ID", "日時", "分類", "操作", "対象種別", "対象ID", "変更内容"}); err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var createdAt, category, action, subjectType, subjectID, details string
		if err := rows.Scan(&id, &createdAt, &category, &action, &subjectType, &subjectID, &details); err != nil {
			return err
		}
		if err := writer.Write([]string{
			strconv.FormatInt(id, 10), formatLogTime(createdAt), operationCategoryLabel(category), operationActionLabel(action), subjectType, subjectID, details,
		}); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	writer.Flush()
	return writer.Error()
}

func (app *App) auditEventName() string {
	if eventName := strings.TrimSpace(app.eventName); eventName != "" {
		return eventName
	}
	if storeName := app.catalogSnapshot().Store.Name; storeName != "" {
		return storeName
	}
	return "未指定"
}

func operationCategoryLabel(category string) string {
	if category == "inventory" {
		return "在庫"
	}
	if category == "settings" {
		return "設定"
	}
	return category
}

func operationActionLabel(action string) string {
	labels := map[string]string{
		"inventory_adjustment":          "在庫数を手動変更",
		"product_create":                "商品を追加",
		"product_update":                "商品を変更",
		"product_delete":                "商品を削除",
		"set_discounts_replace":         "セット割引を変更",
		"audit_output_directory_update": "監査CSV出力先を変更",
	}
	if label, exists := labels[action]; exists {
		return label
	}
	return action
}

func (app *App) salesSummary() ([]salesSummaryItem, int, error) {
	rows, err := app.db.Query(`
		SELECT product_id, product_name, SUM(quantity)
		FROM sale_items
		GROUP BY product_id, product_name
		HAVING SUM(quantity) != 0
		ORDER BY product_id, product_name`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]salesSummaryItem, 0)
	for rows.Next() {
		var item salesSummaryItem
		if err := rows.Scan(&item.ProductID, &item.Name, &item.Quantity); err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var total int
	if err := app.db.QueryRow(`SELECT COALESCE(SUM(total), 0) FROM sales`).Scan(&total); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func salesLimit(r *http.Request) (int, error) {
	const defaultLimit = 10
	const maxLimit = 100
	value := r.URL.Query().Get("limit")
	if value == "" {
		return defaultLimit, nil
	}
	limit, err := strconv.Atoi(value)
	if err != nil || limit < 1 || limit > maxLimit {
		return 0, fmt.Errorf("limitは1〜%dの整数で指定してください", maxLimit)
	}
	return limit, nil
}

func formatLogTime(value string) string {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return value
	}
	jst := time.FixedZone("JST", 9*60*60)
	return parsed.In(jst).Format("2006-01-02 15:04:05")
}

func ensureSingleJSONValue(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("複数のJSON値が含まれています")
	}
	return nil
}

func calculateSale(catalog ProductCatalog, items []checkoutItem) (calculatedSale, error) {
	if len(items) == 0 {
		return calculatedSale{}, errors.New("商品を1件以上選択してください")
	}
	productsByID := make(map[string]Product, len(catalog.Products))
	for _, product := range catalog.Products {
		productsByID[product.ID] = product
	}
	quantities := make(map[string]int, len(items))
	for _, item := range items {
		if _, ok := productsByID[item.ProductID]; !ok {
			return calculatedSale{}, fmt.Errorf("未登録の商品です: %s", item.ProductID)
		}
		if item.Quantity <= 0 {
			return calculatedSale{}, errors.New("数量は1以上にしてください")
		}
		quantities[item.ProductID] += item.Quantity
	}

	lines := make([]saleLine, 0, len(quantities))
	var subtotal int
	for _, product := range catalog.Products {
		quantity := quantities[product.ID]
		if quantity == 0 {
			continue
		}
		lineSubtotal := product.UnitPrice * quantity
		lines = append(lines, saleLine{
			ProductID: product.ID, Name: product.Name, UnitPrice: product.UnitPrice, Quantity: quantity,
			Subtotal: lineSubtotal, Discount: 0, Total: lineSubtotal,
		})
		subtotal += lineSubtotal
	}
	appliedSets, discount := calculateSetDiscounts(catalog, quantities)
	return calculatedSale{
		Lines: lines, AppliedSets: appliedSets, Subtotal: subtotal, Discount: discount, Total: subtotal - discount,
	}, nil
}

type compiledSetDiscount struct {
	discount     SetDiscount
	requirements []int
	saving       int
}

type discountPlan struct {
	discount     int
	applications []int
}

func calculateSetDiscounts(catalog ProductCatalog, quantities map[string]int) ([]appliedSetUsage, int) {
	productIndexes := make(map[string]int, len(catalog.Products))
	remaining := make([]int, len(catalog.Products))
	for index, product := range catalog.Products {
		productIndexes[product.ID] = index
		remaining[index] = quantities[product.ID]
	}
	rules := make([]compiledSetDiscount, len(catalog.SetDiscounts))
	for index, discount := range catalog.SetDiscounts {
		rule := compiledSetDiscount{discount: discount, requirements: make([]int, len(catalog.Products))}
		regularPrice := 0
		for _, item := range discount.Items {
			productIndex := productIndexes[item.ProductID]
			rule.requirements[productIndex] = item.Quantity
			regularPrice += catalog.Products[productIndex].UnitPrice * item.Quantity
		}
		rule.saving = regularPrice - discount.SetPrice
		rules[index] = rule
	}

	memo := make(map[string]discountPlan)
	var solve func([]int) discountPlan
	solve = func(available []int) discountPlan {
		key := quantityKey(available)
		if plan, exists := memo[key]; exists {
			return plan
		}
		best := discountPlan{applications: make([]int, len(rules))}
		for ruleIndex, rule := range rules {
			canApply := true
			next := make([]int, len(available))
			copy(next, available)
			for productIndex, required := range rule.requirements {
				if required > next[productIndex] {
					canApply = false
					break
				}
				next[productIndex] -= required
			}
			if !canApply {
				continue
			}
			candidate := solve(next)
			candidate.discount += rule.saving
			candidate.applications = append([]int(nil), candidate.applications...)
			candidate.applications[ruleIndex]++
			if candidate.discount > best.discount {
				best = candidate
			}
		}
		memo[key] = best
		return best
	}

	plan := solve(remaining)
	appliedSets := make([]appliedSetUsage, 0, len(rules))
	for index, count := range plan.applications {
		if count == 0 {
			continue
		}
		rule := rules[index]
		appliedSets = append(appliedSets, appliedSetUsage{
			SetID: rule.discount.ID, Name: rule.discount.Name, Quantity: count,
			SetPrice: rule.discount.SetPrice, Discount: rule.saving * count,
		})
	}
	return appliedSets, plan.discount
}

func quantityKey(quantities []int) string {
	values := make([]string, len(quantities))
	for index, quantity := range quantities {
		values[index] = strconv.Itoa(quantity)
	}
	return strings.Join(values, ",")
}

func (app *App) saveSale(sale calculatedSale) (int64, error) {
	return app.saveSaleWithCorrection(sale, nil)
}

func (app *App) saveSaleWithCorrection(sale calculatedSale, correctionAdjustmentID *int64) (int64, error) {
	tx, err := app.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if correctionAdjustmentID != nil {
		var adjustmentType, status string
		if err := tx.QueryRow(
			`SELECT adjustment_type, status FROM sale_adjustments WHERE id = ?`, *correctionAdjustmentID,
		).Scan(&adjustmentType, &status); err != nil || adjustmentType != "correction" || status != "pending" {
			return 0, &invalidCorrectionError{}
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	for _, line := range sale.Lines {
		result, err := tx.Exec(`
			UPDATE inventory
			SET quantity = quantity - ?, updated_at = ?
			WHERE product_id = ? AND quantity >= ?`,
			line.Quantity, now, line.ProductID, line.Quantity,
		)
		if err != nil {
			return 0, err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if rowsAffected != 1 {
			return 0, &insufficientStockError{ProductName: line.Name}
		}
		if _, err := tx.Exec(`
			INSERT INTO inventory_movements (product_id, delta, reason, created_at) VALUES (?, ?, ?, ?)`,
			line.ProductID, -line.Quantity, "sale", now,
		); err != nil {
			return 0, err
		}
	}
	result, err := tx.Exec(
		`INSERT INTO sales (created_at, subtotal, discount, total) VALUES (?, ?, ?, ?)`,
		now, sale.Subtotal, sale.Discount, sale.Total,
	)
	if err != nil {
		return 0, err
	}
	saleID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	statement, err := tx.Prepare(`
		INSERT INTO sale_items (sale_id, product_id, product_name, unit_price, quantity, subtotal, discount, total)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer statement.Close()
	for _, line := range sale.Lines {
		if _, err := statement.Exec(saleID, line.ProductID, line.Name, line.UnitPrice, line.Quantity, line.Subtotal, line.Discount, line.Total); err != nil {
			return 0, err
		}
	}
	setStatement, err := tx.Prepare(`
		INSERT INTO sale_set_discounts (sale_id, set_id, set_name, quantity, set_price, discount)
		VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer setStatement.Close()
	for _, appliedSet := range sale.AppliedSets {
		if _, err := setStatement.Exec(saleID, appliedSet.SetID, appliedSet.Name, appliedSet.Quantity, appliedSet.SetPrice, appliedSet.Discount); err != nil {
			return 0, err
		}
	}
	if correctionAdjustmentID != nil {
		result, err := tx.Exec(`
			UPDATE sale_adjustments
			SET replacement_sale_id = ?, status = 'completed', completed_at = ?
			WHERE id = ? AND adjustment_type = 'correction' AND status = 'pending'`,
			saleID, now, *correctionAdjustmentID,
		)
		if err != nil {
			return 0, err
		}
		rowsAffected, err := result.RowsAffected()
		if err != nil || rowsAffected != 1 {
			return 0, &invalidCorrectionError{}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return saleID, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("JSON応答の書き込みに失敗しました: %v", err)
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func methodNotAllowed(w http.ResponseWriter, allowed string) {
	w.Header().Set("Allow", allowed)
	writeError(w, http.StatusMethodNotAllowed, "このメソッドは利用できません")
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.Path, time.Since(started).Round(time.Millisecond))
	})
}

func parsePort(value string) (int, error) {
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("PORTは1〜65535の整数で指定してください")
	}
	return port, nil
}
