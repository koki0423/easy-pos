package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCalculateSaleAppliesSetDiscountAndMergesItems(t *testing.T) {
	catalog := ProductCatalog{
		Products: []Product{
			{ID: "coffee", Name: "コーヒー", UnitPrice: 450},
			{ID: "croissant", Name: "クロワッサン", UnitPrice: 320},
		},
		SetDiscounts: []SetDiscount{
			{ID: "coffee-pair", Name: "コーヒー2杯セット", Items: []SetDiscountItem{{ProductID: "coffee", Quantity: 2}}, SetPrice: 800},
			{ID: "morning", Name: "モーニングセット", Items: []SetDiscountItem{{ProductID: "coffee", Quantity: 1}, {ProductID: "croissant", Quantity: 1}}, SetPrice: 650},
		},
	}
	if err := validateCatalog(catalog); err != nil {
		t.Fatalf("validateCatalog returned error: %v", err)
	}
	sale, err := calculateSale(catalog, []checkoutItem{
		{ProductID: "coffee", Quantity: 2},
		{ProductID: "croissant", Quantity: 1},
		{ProductID: "coffee", Quantity: 1},
	})
	if err != nil {
		t.Fatalf("calculateSale returned error: %v", err)
	}
	if sale.Subtotal != 1670 || sale.Discount != 220 || sale.Total != 1450 {
		t.Fatalf("unexpected totals: %#v", sale)
	}
	if len(sale.Lines) != 2 || sale.Lines[0].Quantity != 3 {
		t.Fatalf("unexpected lines: %#v", sale.Lines)
	}
	if len(sale.AppliedSets) != 2 || sale.AppliedSets[0].SetID != "coffee-pair" || sale.AppliedSets[1].SetID != "morning" {
		t.Fatalf("unexpected applied sets: %#v", sale.AppliedSets)
	}
}

func TestCalculateSaleRejectsUnknownProduct(t *testing.T) {
	_, err := calculateSale(ProductCatalog{}, []checkoutItem{{ProductID: "missing", Quantity: 1}})
	if err == nil {
		t.Fatal("expected an error for an unknown product")
	}
}

func TestProductsYAMLLoads(t *testing.T) {
	if _, err := loadCatalog("products.yaml"); err != nil {
		t.Fatalf("products.yaml could not be loaded: %v", err)
	}
}

func TestParseRunConfig(t *testing.T) {
	if _, err := parseRunConfig(nil); err == nil {
		t.Fatal("expected an error when no mode is supplied")
	}
	deploy, err := parseRunConfig([]string{"--deploy"})
	if err != nil || deploy.ResetOnShutdown || !deploy.ExportCatalogOnShutdown || !deploy.SelectDatabase {
		t.Fatalf("unexpected deploy config: %#v, error=%v", deploy, err)
	}
	testMode, err := parseRunConfig([]string{"--test"})
	if err != nil || !testMode.ResetOnShutdown || testMode.ExportCatalogOnShutdown || testMode.SelectDatabase {
		t.Fatalf("unexpected test config: %#v, error=%v", testMode, err)
	}
}

func TestResetSQLiteDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "temporary.db")
	for _, candidate := range []string{path, path + "-shm", path + "-wal"} {
		if err := os.WriteFile(candidate, []byte("test"), 0o600); err != nil {
			t.Fatalf("could not prepare %s: %v", candidate, err)
		}
	}
	if err := resetSQLiteDatabase(path); err != nil {
		t.Fatalf("resetSQLiteDatabase returned error: %v", err)
	}
	for _, candidate := range []string{path, path + "-shm", path + "-wal"} {
		if _, err := os.Stat(candidate); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("expected %s to be removed, got %v", candidate, err)
		}
	}
}

func TestSaveSaleConsumesStockAndRollsBackWhenStockIsInsufficient(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{
		{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 2},
		{ID: "card", Name: "カード", UnitPrice: 100, InitialStock: 1},
	}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}
	sale, err := calculateSale(catalog, []checkoutItem{{ProductID: "book", Quantity: 1}, {ProductID: "card", Quantity: 1}})
	if err != nil {
		t.Fatalf("calculateSale returned error: %v", err)
	}
	if _, err := app.saveSale(sale); err != nil {
		t.Fatalf("saveSale returned error: %v", err)
	}
	assertStock(t, db, "book", 1)
	assertStock(t, db, "card", 0)

	_, err = app.saveSale(sale)
	var stockError *insufficientStockError
	if !errors.As(err, &stockError) {
		t.Fatalf("expected insufficientStockError, got %v", err)
	}
	assertStock(t, db, "book", 1)
	assertStock(t, db, "card", 0)
}

func TestInventoryUpdateAPI(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 2}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}
	request := httptest.NewRequest(http.MethodPut, "/api/inventory/book", strings.NewReader(`{"quantity": 7}`))
	response := httptest.NewRecorder()
	app.handleInventoryItem(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d, body=%s", response.Code, response.Body.String())
	}
	assertStock(t, db, "book", 7)
}

func TestSettingsProductSetAndYAMLExport(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}

	productRequest := httptest.NewRequest(http.MethodPut, "/api/settings/products/card", strings.NewReader(`{"name":"カード","unit_price":100,"stock":0}`))
	productResponse := httptest.NewRecorder()
	app.handleSettingsProduct(productResponse, productRequest)
	if productResponse.Code != http.StatusOK {
		t.Fatalf("unexpected product settings status: %d body=%s", productResponse.Code, productResponse.Body.String())
	}
	assertStock(t, db, "card", 0)

	setRequest := httptest.NewRequest(http.MethodPut, "/api/settings/set-discounts", strings.NewReader(`{"set_discounts":[{"id":"book-card","name":"本とカード","items":[{"product_id":"book","quantity":1},{"product_id":"card","quantity":1}],"set_price":550}]}`))
	setResponse := httptest.NewRecorder()
	app.handleSettingsSetDiscounts(setResponse, setRequest)
	if setResponse.Code != http.StatusOK {
		t.Fatalf("unexpected set settings status: %d body=%s", setResponse.Code, setResponse.Body.String())
	}

	outputPath := filepath.Join(t.TempDir(), "products.yaml")
	if err := os.WriteFile(outputPath, []byte("store: {name: old}\nproducts: []\n"), 0o600); err != nil {
		t.Fatalf("could not prepare YAML export target: %v", err)
	}
	if err := app.exportCatalog(outputPath); err != nil {
		t.Fatalf("exportCatalog returned error: %v", err)
	}
	exported, err := loadCatalog(outputPath)
	if err != nil {
		t.Fatalf("exported YAML could not be loaded: %v", err)
	}
	card, exists := productMap(exported.Products)["card"]
	if !exists || card.InitialStock != 0 {
		t.Fatalf("zero-stock product was not exported: %#v", exported.Products)
	}
	if len(exported.SetDiscounts) != 1 || exported.SetDiscounts[0].ID != "book-card" {
		t.Fatalf("set discount was not exported: %#v", exported.SetDiscounts)
	}
}

func TestAuditExportWritesConfiguredDirectory(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}
	sale, err := calculateSale(catalog, []checkoutItem{{ProductID: "book", Quantity: 1}})
	if err != nil {
		t.Fatalf("could not calculate test sale: %v", err)
	}
	if _, err := app.saveSale(sale); err != nil {
		t.Fatalf("could not save test sale: %v", err)
	}
	directory := t.TempDir()
	settingsRequest := httptest.NewRequest(http.MethodPut, "/api/settings/audit", strings.NewReader(fmt.Sprintf(`{"output_directory":%q}`, directory)))
	settingsResponse := httptest.NewRecorder()
	app.handleSettingsAudit(settingsResponse, settingsRequest)
	if settingsResponse.Code != http.StatusOK {
		t.Fatalf("unexpected audit settings status: %d body=%s", settingsResponse.Code, settingsResponse.Body.String())
	}
	exportRequest := httptest.NewRequest(http.MethodPost, "/api/audit/export", nil)
	exportResponse := httptest.NewRecorder()
	app.handleAuditExport(exportResponse, exportRequest)
	if exportResponse.Code != http.StatusCreated {
		t.Fatalf("unexpected audit export status: %d body=%s", exportResponse.Code, exportResponse.Body.String())
	}
	var result auditExportResponse
	if err := json.Unmarshal(exportResponse.Body.Bytes(), &result); err != nil {
		t.Fatalf("could not decode audit export response: %v", err)
	}
	contents, err := os.ReadFile(result.Path)
	if err != nil || !strings.Contains(string(contents), "商品名") || !strings.Contains(string(contents), "本") {
		t.Fatalf("unexpected audit CSV: path=%s error=%v content=%q", result.Path, err, contents)
	}
}

func TestOperationsAuditCSVIncludesInventoryAndSettingsChanges(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db, eventName: "event-a"}

	inventoryRequest := httptest.NewRequest(http.MethodPut, "/api/inventory/book", strings.NewReader(`{"quantity":3}`))
	inventoryResponse := httptest.NewRecorder()
	app.handleInventoryItem(inventoryResponse, inventoryRequest)
	if inventoryResponse.Code != http.StatusOK {
		t.Fatalf("could not update inventory: status=%d body=%s", inventoryResponse.Code, inventoryResponse.Body.String())
	}
	productRequest := httptest.NewRequest(http.MethodPut, "/api/settings/products/book", strings.NewReader(`{"name":"改訂版の本","unit_price":600,"stock":3}`))
	productResponse := httptest.NewRecorder()
	app.handleSettingsProduct(productResponse, productRequest)
	if productResponse.Code != http.StatusOK {
		t.Fatalf("could not update product settings: status=%d body=%s", productResponse.Code, productResponse.Body.String())
	}
	operationsDirectory := t.TempDir()
	directoryRequest := httptest.NewRequest(http.MethodPut, "/api/settings/audit", strings.NewReader(fmt.Sprintf(`{"output_directory":%q}`, operationsDirectory)))
	directoryResponse := httptest.NewRecorder()
	app.handleSettingsAudit(directoryResponse, directoryRequest)
	if directoryResponse.Code != http.StatusOK {
		t.Fatalf("could not update audit settings: status=%d body=%s", directoryResponse.Code, directoryResponse.Body.String())
	}

	var output bytes.Buffer
	if err := app.writeOperationsCSV(&output); err != nil {
		t.Fatalf("writeOperationsCSV returned error: %v", err)
	}
	contents := output.String()
	for _, expected := range []string{"イベント名,event-a", "在庫数を手動変更", "商品を変更", "監査CSV出力先を変更", "改訂版の本"} {
		if !strings.Contains(contents, expected) {
			t.Fatalf("operations audit CSV is missing %q: %q", expected, contents)
		}
	}
	exportRequest := httptest.NewRequest(http.MethodPost, "/api/audit/operations/export", nil)
	exportResponse := httptest.NewRecorder()
	app.handleOperationsAuditExport(exportResponse, exportRequest)
	if exportResponse.Code != http.StatusCreated {
		t.Fatalf("unexpected operations audit export status: %d body=%s", exportResponse.Code, exportResponse.Body.String())
	}
	var exportResult auditExportResponse
	if err := json.Unmarshal(exportResponse.Body.Bytes(), &exportResult); err != nil {
		t.Fatalf("could not decode operations audit export response: %v", err)
	}
	exportedContents, err := os.ReadFile(exportResult.Path)
	if err != nil || !strings.Contains(string(exportedContents), "在庫数を手動変更") {
		t.Fatalf("unexpected operations audit file: path=%s error=%v content=%q", exportResult.Path, err, exportedContents)
	}
}

func TestEventManagerCreatesAndReopensIsolatedEventDatabase(t *testing.T) {
	directory := t.TempDir()
	template := filepath.Join(directory, "products.yaml")
	if err := os.WriteFile(template, []byte(`store:
  name: テスト店
products:
  - id: book
    name: 本
    unit_price: 500
    initial_stock: 2
set_discounts: []
`), 0o600); err != nil {
		t.Fatalf("could not write event template: %v", err)
	}
	config := runConfig{Mode: "本番", DatabaseFile: filepath.Join(directory, "legacy.db"), ExportCatalogOnShutdown: true, SelectDatabase: true}
	manager := &eventManager{config: config, productsTemplate: template, eventsDirectory: filepath.Join(directory, "events")}
	record, err := manager.create("event-a")
	if err != nil {
		t.Fatalf("could not create event: %v", err)
	}
	if record.ID != "event-a" || manager.currentApp() == nil {
		t.Fatalf("event was not opened: %#v", record)
	}
	if products := manager.currentApp().catalogSnapshot().Products; len(products) != 0 {
		t.Fatalf("new event should start with no products, got %#v", products)
	}
	emptyCatalogRequest := httptest.NewRequest(http.MethodGet, "/api/settings/catalog", nil)
	emptyCatalogResponse := httptest.NewRecorder()
	manager.currentApp().handleSettingsCatalog(emptyCatalogResponse, emptyCatalogRequest)
	if emptyCatalogResponse.Code != http.StatusOK || !strings.Contains(emptyCatalogResponse.Body.String(), `"products":[]`) {
		t.Fatalf("empty event catalog should return an empty product array: status=%d body=%s", emptyCatalogResponse.Code, emptyCatalogResponse.Body.String())
	}
	if want := filepath.Join(manager.eventsDirectory, "event-a", "pos.db"); record.DatabaseFile != want {
		t.Fatalf("unexpected event database path: got %s want %s", record.DatabaseFile, want)
	}
	if want := filepath.Join(manager.eventsDirectory, "event-a", "products.yaml"); record.ProductsFile != want {
		t.Fatalf("unexpected event YAML path: got %s want %s", record.ProductsFile, want)
	}
	app := manager.currentApp()
	request := httptest.NewRequest(http.MethodPut, "/api/settings/products/book", strings.NewReader(`{"name":"本","unit_price":500,"stock":0}`))
	response := httptest.NewRecorder()
	app.handleSettingsProduct(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("could not update event inventory: status=%d body=%s", response.Code, response.Body.String())
	}
	if err := manager.close(); err != nil {
		t.Fatalf("could not close event manager: %v", err)
	}
	exported, err := loadCatalog(record.ProductsFile)
	if err != nil {
		t.Fatalf("could not load event YAML: %v", err)
	}
	if got := productMap(exported.Products)["book"].InitialStock; got != 0 {
		t.Fatalf("event stock was not persisted to event YAML: got %d", got)
	}

	reopened := &eventManager{config: config, productsTemplate: template, eventsDirectory: filepath.Join(directory, "events")}
	found, err := reopened.findEvent("event-a")
	if err != nil {
		t.Fatalf("could not find created event: %v", err)
	}
	if err := reopened.open(found); err != nil {
		t.Fatalf("could not reopen event: %v", err)
	}
	defer reopened.close()
	assertStock(t, reopened.currentApp().db, "book", 0)
}

func TestSalesLogAndCSVExport(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db, eventName: "event-a"}
	sale, err := calculateSale(catalog, []checkoutItem{{ProductID: "book", Quantity: 1}})
	if err != nil {
		t.Fatalf("calculateSale returned error: %v", err)
	}
	if _, err := app.saveSale(sale); err != nil {
		t.Fatalf("saveSale returned error: %v", err)
	}

	logRequest := httptest.NewRequest(http.MethodGet, "/api/sales?limit=10", nil)
	logResponse := httptest.NewRecorder()
	app.handleSales(logResponse, logRequest)
	if logResponse.Code != http.StatusOK || !strings.Contains(logResponse.Body.String(), "本") {
		t.Fatalf("unexpected sales log response: status=%d body=%s", logResponse.Code, logResponse.Body.String())
	}

	csvRequest := httptest.NewRequest(http.MethodGet, "/api/sales/export.csv", nil)
	csvResponse := httptest.NewRecorder()
	app.handleSalesCSV(csvResponse, csvRequest)
	if csvResponse.Code != http.StatusOK || !strings.Contains(csvResponse.Body.String(), "イベント名,event-a") || !strings.Contains(csvResponse.Body.String(), "出力日時") || !strings.Contains(csvResponse.Body.String(), "純売上集計") || !strings.Contains(csvResponse.Body.String(), "合計売上（円）,500") || !strings.Contains(csvResponse.Body.String(), "商品名") || !strings.Contains(csvResponse.Body.String(), "本") {
		t.Fatalf("unexpected CSV response: status=%d body=%q", csvResponse.Code, csvResponse.Body.String())
	}
}

func TestVoidCreatesAnImmutableReversalAndRestoresStock(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}
	original := mustSaveTestSale(t, app, catalog)
	assertStock(t, db, "book", 0)

	adjustment, err := app.createSaleAdjustment(original, adjustmentRequest{Type: "void", Reason: "商品選択の誤り"})
	if err != nil {
		t.Fatalf("createSaleAdjustment returned error: %v", err)
	}
	if adjustment.AdjustmentSaleID == original || adjustment.Type != "void" {
		t.Fatalf("unexpected adjustment: %#v", adjustment)
	}
	assertStock(t, db, "book", 1)

	var originalTotal, reversalTotal int
	if err := db.QueryRow(`SELECT total FROM sales WHERE id = ?`, original).Scan(&originalTotal); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT total FROM sales WHERE id = ?`, adjustment.AdjustmentSaleID).Scan(&reversalTotal); err != nil {
		t.Fatal(err)
	}
	if originalTotal != 500 || reversalTotal != -500 {
		t.Fatalf("unexpected ledger totals: original=%d reversal=%d", originalTotal, reversalTotal)
	}
	var adjustmentType, status string
	if err := db.QueryRow(`SELECT adjustment_type, status FROM sale_adjustments WHERE id = ?`, adjustment.AdjustmentID).Scan(&adjustmentType, &status); err != nil {
		t.Fatal(err)
	}
	if adjustmentType != "void" || status != "completed" {
		t.Fatalf("unexpected adjustment state: type=%s status=%s", adjustmentType, status)
	}
	if _, err := app.createSaleAdjustment(original, adjustmentRequest{Type: "void", Reason: "重複会計"}); err == nil {
		t.Fatal("expected a second adjustment to be rejected")
	}
	logRequest := httptest.NewRequest(http.MethodGet, "/api/sales?limit=10", nil)
	logResponse := httptest.NewRecorder()
	app.handleSales(logResponse, logRequest)
	if logResponse.Code != http.StatusOK || !strings.Contains(logResponse.Body.String(), `"adjustment_type":"void"`) {
		t.Fatalf("unexpected adjusted sales log: status=%d body=%s", logResponse.Code, logResponse.Body.String())
	}
	csvRequest := httptest.NewRequest(http.MethodGet, "/api/sales/export.csv", nil)
	csvResponse := httptest.NewRecorder()
	app.handleSalesCSV(csvResponse, csvRequest)
	if csvResponse.Code != http.StatusOK || !strings.Contains(csvResponse.Body.String(), "void") || !strings.Contains(csvResponse.Body.String(), "商品選択の誤り") || !strings.Contains(csvResponse.Body.String(), "合計売上（円）,0") {
		t.Fatalf("unexpected adjusted CSV: status=%d body=%q", csvResponse.Code, csvResponse.Body.String())
	}
}

func TestCorrectionLinksTheReplacementSale(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}
	original := mustSaveTestSale(t, app, catalog)
	adjustment, err := app.createSaleAdjustment(original, adjustmentRequest{Type: "correction", Reason: "数量入力の誤り"})
	if err != nil {
		t.Fatalf("createSaleAdjustment returned error: %v", err)
	}
	if len(adjustment.DraftItems) != 1 || adjustment.DraftItems[0].ProductID != "book" {
		t.Fatalf("unexpected correction draft: %#v", adjustment.DraftItems)
	}
	pendingDraft, err := app.pendingCorrectionDraft(original)
	if err != nil || pendingDraft.AdjustmentID != adjustment.AdjustmentID {
		t.Fatalf("unexpected pending correction draft: %#v, error=%v", pendingDraft, err)
	}
	correctedSale, err := calculateSale(catalog, adjustment.DraftItems)
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := app.saveSaleWithCorrection(correctedSale, &adjustment.AdjustmentID)
	if err != nil {
		t.Fatalf("saveSaleWithCorrection returned error: %v", err)
	}
	var status string
	var linkedReplacement int64
	if err := db.QueryRow(`SELECT status, replacement_sale_id FROM sale_adjustments WHERE id = ?`, adjustment.AdjustmentID).Scan(&status, &linkedReplacement); err != nil {
		t.Fatal(err)
	}
	if status != "completed" || linkedReplacement != replacement {
		t.Fatalf("unexpected correction link: status=%s replacement=%d", status, linkedReplacement)
	}
	if _, err := app.pendingCorrectionDraft(original); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expected no pending correction after completion, got %v", err)
	}
	assertStock(t, db, "book", 0)
}

func TestSaleAdjustmentAPI(t *testing.T) {
	catalog := ProductCatalog{Products: []Product{{ID: "book", Name: "本", UnitPrice: 500, InitialStock: 1}}}
	db, err := openDatabase(filepath.Join(t.TempDir(), "pos.db"))
	if err != nil {
		t.Fatalf("openDatabase returned error: %v", err)
	}
	defer db.Close()
	if err := syncInventory(db, catalog.Products); err != nil {
		t.Fatalf("syncInventory returned error: %v", err)
	}
	app := &App{catalog: catalog, byID: productMap(catalog.Products), db: db}
	original := mustSaveTestSale(t, app, catalog)
	request := httptest.NewRequest(
		http.MethodPost,
		fmt.Sprintf("/api/sales/%d/adjustments", original),
		strings.NewReader(`{"type":"void","reason":"重複会計"}`),
	)
	response := httptest.NewRecorder()
	app.handleSaleAction(response, request)
	if response.Code != http.StatusCreated || !strings.Contains(response.Body.String(), `"type":"void"`) {
		t.Fatalf("unexpected adjustment response: status=%d body=%s", response.Code, response.Body.String())
	}
}

func mustSaveTestSale(t *testing.T, app *App, catalog ProductCatalog) int64 {
	t.Helper()
	sale, err := calculateSale(catalog, []checkoutItem{{ProductID: "book", Quantity: 1}})
	if err != nil {
		t.Fatal(err)
	}
	saleID, err := app.saveSale(sale)
	if err != nil {
		t.Fatal(err)
	}
	return saleID
}

func assertStock(t *testing.T, db *sql.DB, productID string, want int) {
	t.Helper()
	var got int
	if err := db.QueryRow(`SELECT quantity FROM inventory WHERE product_id = ?`, productID).Scan(&got); err != nil {
		t.Fatalf("could not read stock for %s: %v", productID, err)
	}
	if got != want {
		t.Fatalf("unexpected stock for %s: got %d, want %d", productID, got, want)
	}
}
