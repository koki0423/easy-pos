const state = {
  products: [],
  setDiscounts: [],
  inventory: new Map(),
  quantities: new Map(),
	lastSales: [],
  busy: false,
  inventoryBusy: new Set(),
  correctionAdjustmentID: null,
	correctionOriginalSaleID: null,
	selectedSale: null,
};

const productList = document.querySelector("#product-list");
const cartList = document.querySelector("#cart-list");
const subtotalElement = document.querySelector("#subtotal");
const discountElement = document.querySelector("#discount");
const totalElement = document.querySelector("#total");
const appliedSetsElement = document.querySelector("#applied-sets");
const checkoutButton = document.querySelector("#checkout");
const clearButton = document.querySelector("#clear-cart");
const notice = document.querySelector("#notice");
const salesList = document.querySelector("#sales-list");
const refreshSalesButton = document.querySelector("#refresh-sales");
const correctionBanner = document.querySelector("#correction-banner");
const adjustmentDialog = document.querySelector("#adjustment-dialog");
const adjustmentForm = document.querySelector("#adjustment-form");
const adjustmentSaleSummary = document.querySelector("#adjustment-sale-summary");
const adjustmentReason = document.querySelector("#adjustment-reason");
const adjustmentOther = document.querySelector("#adjustment-other");
const adjustmentOtherField = document.querySelector("#adjustment-other-field");
const adjustmentCartWarning = document.querySelector("#adjustment-cart-warning");
const adjustmentError = document.querySelector("#adjustment-error");
const adjustmentSubmit = document.querySelector("#adjustment-submit");
const adjustmentCancel = document.querySelector("#adjustment-cancel");
const posHeader = document.querySelector("#pos-header");
const posLayout = document.querySelector("#pos-layout");
const eventSelectionDialog = document.querySelector("#event-selection-dialog");
const eventCreateForm = document.querySelector("#event-create-form");
const eventCreateID = document.querySelector("#event-create-id");
const eventList = document.querySelector("#event-list");
const eventSelectionError = document.querySelector("#event-selection-error");

const yen = new Intl.NumberFormat("ja-JP", { style: "currency", currency: "JPY", maximumFractionDigits: 0 });
const amount = (value) => yen.format(value);

function getQuantity(productID) { return state.quantities.get(productID) || 0; }
function getStock(productID) { return state.inventory.get(productID) ?? 0; }

function showNotice(message, isError = false) {
  notice.textContent = message;
  notice.className = isError ? "notice error" : "notice";
}

function setQuantity(productID, quantity) {
  if (quantity <= 0) state.quantities.delete(productID);
  else state.quantities.set(productID, quantity);
  showNotice("");
  renderProducts();
  renderCart();
}

function calculateOrder() {
  const quantities = state.products.map((product) => getQuantity(product.id));
  const subtotal = state.products.reduce(
    (sum, product, index) => sum + product.unit_price * quantities[index],
    0,
  );
  const { discount, appliedSets } = calculateSetDiscounts(quantities);
  return { subtotal, discount, total: subtotal - discount, appliedSets };
}

function calculateSetDiscounts(initialQuantities) {
  const productIndexes = new Map(state.products.map((product, index) => [product.id, index]));
  const rules = state.setDiscounts.map((set) => {
    const requirements = new Array(state.products.length).fill(0);
    let regularPrice = 0;
    for (const item of set.items) {
      const productIndex = productIndexes.get(item.product_id);
      requirements[productIndex] = item.quantity;
      regularPrice += state.products[productIndex].unit_price * item.quantity;
    }
    return { ...set, requirements, saving: regularPrice - set.set_price };
  });
  const memo = new Map();

  function solve(available) {
    const key = available.join(",");
    if (memo.has(key)) return memo.get(key);
    let best = { discount: 0, applications: new Array(rules.length).fill(0) };
    rules.forEach((rule, ruleIndex) => {
      const next = [...available];
      const canApply = rule.requirements.every((required, productIndex) => {
        if (required > next[productIndex]) return false;
        next[productIndex] -= required;
        return true;
      });
      if (!canApply) return;
      const child = solve(next);
      const candidate = {
        discount: child.discount + rule.saving,
        applications: [...child.applications],
      };
      candidate.applications[ruleIndex] += 1;
      if (candidate.discount > best.discount) best = candidate;
    });
    memo.set(key, best);
    return best;
  }

  const plan = solve(initialQuantities);
  const appliedSets = plan.applications.flatMap((count, index) => {
    if (count === 0) return [];
    const rule = rules[index];
    return [{ ...rule, quantity: count, discount: rule.saving * count }];
  });
  return { discount: plan.discount, appliedSets };
}

function renderProducts() {
  productList.replaceChildren(...state.products.map((product) => {
    const card = document.createElement("article");
    card.className = "product-card";
    const stock = getStock(product.id);
    const inCart = getQuantity(product.id);
    const isUpdatingStock = state.inventoryBusy.has(product.id);
    const setNames = state.setDiscounts
      .filter((set) => set.items.some((item) => item.product_id === product.id))
      .map((set) => set.name);
    const discountText = setNames.length > 0 ? `対象セット: ${setNames.join("・")}` : "対象セットなし";
    const stockText = stock > 0 ? `在庫 ${stock}` : "在庫なし";
    card.innerHTML = `
      <h3>${escapeHTML(product.name)}</h3>
      <p class="price">${amount(product.unit_price)}</p>
      <p class="set-label">${escapeHTML(discountText)}</p>
      <p class="stock-label ${stock === 0 ? "is-empty" : ""}">${stockText}</p>
      <button class="add-button" type="button" data-product-id="${escapeHTML(product.id)}" ${state.busy || inCart >= stock ? "disabled" : ""}>追加</button>
      <details class="inventory-control">
        <summary>在庫を管理</summary>
        <div class="inventory-form">
          <label>在庫数 <input type="number" min="0" step="1" value="${stock}" inputmode="numeric" /></label>
          <button type="button" data-action="update-stock" data-product-id="${escapeHTML(product.id)}" ${isUpdatingStock || state.busy ? "disabled" : ""}>${isUpdatingStock ? "更新中…" : "更新"}</button>
        </div>
      </details>`;
    return card;
  }));
}

function renderCart() {
  const selected = state.products.filter((product) => getQuantity(product.id) > 0);
  renderCorrectionBanner();
  clearButton.disabled = selected.length === 0 || state.busy;
  checkoutButton.disabled = selected.length === 0 || state.busy;

  if (selected.length === 0) {
    cartList.innerHTML = '<p class="empty-cart">商品を選んでください</p>';
    renderAppliedSets([]);
    updateTotals(0, 0, 0);
    return;
  }

  const order = calculateOrder();
  cartList.replaceChildren(...selected.map((product) => {
    const quantity = getQuantity(product.id);
    const lineTotal = product.unit_price * quantity;
    const item = document.createElement("article");
    item.className = "cart-item";
    item.innerHTML = `
      <div class="cart-item__top">
        <div><p class="cart-item__name">${escapeHTML(product.name)}</p>
        <p class="cart-item__detail">${amount(product.unit_price)} × ${quantity}　残り在庫 ${getStock(product.id) - quantity}</p></div>
        <span class="cart-item__total">${amount(lineTotal)}</span>
      </div>
      <div class="cart-item__bottom">
        <div class="quantity-control" aria-label="${escapeHTML(product.name)}の数量">
          <button type="button" data-action="decrease" data-product-id="${escapeHTML(product.id)}" aria-label="${escapeHTML(product.name)}を1点減らす">−</button>
          <span>${quantity}</span>
          <button type="button" data-action="increase" data-product-id="${escapeHTML(product.id)}" aria-label="${escapeHTML(product.name)}を1点増やす" ${quantity >= getStock(product.id) || state.busy ? "disabled" : ""}>＋</button>
        </div>
        <button class="remove-button" type="button" data-action="remove" data-product-id="${escapeHTML(product.id)}">削除</button>
      </div>`;
    return item;
  }));
  renderAppliedSets(order.appliedSets);
  updateTotals(order.subtotal, order.discount, order.total);
}

function renderCorrectionBanner() {
  if (state.correctionAdjustmentID === null) {
    correctionBanner.hidden = true;
    correctionBanner.textContent = "";
    return;
  }
  correctionBanner.hidden = false;
  correctionBanner.textContent = `訂正会計: 会計 #${state.correctionOriginalSaleID} を取消済みです。内容を確認して、正しい会計を確定してください。`;
}

function renderAppliedSets(appliedSets) {
  appliedSetsElement.replaceChildren(...appliedSets.map((set) => {
    const line = document.createElement("div");
    line.textContent = `${set.name} × ${set.quantity}　−${amount(set.discount)}`;
    return line;
  }));
}

function updateTotals(subtotal, discount, total) {
  subtotalElement.textContent = amount(subtotal);
  discountElement.textContent = `−${amount(discount)}`;
  totalElement.textContent = amount(total);
}

function escapeHTML(value) {
  const element = document.createElement("span");
  element.textContent = value;
  return element.innerHTML;
}

async function updateInventory(productID, rawQuantity) {
  const quantity = Number(rawQuantity);
  if (!Number.isInteger(quantity) || quantity < 0) {
    showNotice("在庫数は0以上の整数で入力してください", true);
    return;
  }
  state.inventoryBusy.add(productID);
  renderProducts();
  try {
    const response = await fetch(`/api/inventory/${encodeURIComponent(productID)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ quantity }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "在庫を更新できませんでした");
    state.inventory.set(productID, result.quantity);
    if (getQuantity(productID) > result.quantity) {
      if (result.quantity === 0) state.quantities.delete(productID);
      else state.quantities.set(productID, result.quantity);
      renderCart();
    }
    showNotice(`${result.name} の在庫を ${result.quantity} 点に更新しました`);
  } catch (error) {
    showNotice(error.message || "在庫を更新できませんでした", true);
  } finally {
    state.inventoryBusy.delete(productID);
    renderProducts();
  }
}

productList.addEventListener("click", (event) => {
  const button = event.target.closest("button");
  if (!button || state.busy) return;
  const id = button.dataset.productId;
  if (button.dataset.action === "update-stock") {
    const input = button.closest(".inventory-control").querySelector("input");
    updateInventory(id, input.value);
    return;
  }
  if (id && getQuantity(id) < getStock(id)) setQuantity(id, getQuantity(id) + 1);
});

cartList.addEventListener("click", (event) => {
  const button = event.target.closest("[data-action]");
  if (!button || state.busy) return;
  const id = button.dataset.productId;
  const action = button.dataset.action;
  if (action === "increase" && getQuantity(id) < getStock(id)) setQuantity(id, getQuantity(id) + 1);
  if (action === "decrease") setQuantity(id, getQuantity(id) - 1);
  if (action === "remove") setQuantity(id, 0);
});

clearButton.addEventListener("click", () => {
  if (state.busy) return;
  state.quantities.clear();
  renderProducts();
  renderCart();
});

refreshSalesButton.addEventListener("click", () => {
  loadSales();
});

checkoutButton.addEventListener("click", async () => {
  if (state.busy || state.quantities.size === 0) return;
  state.busy = true;
  renderProducts();
  renderCart();
  showNotice("会計を保存しています…");
  try {
    const items = [...state.quantities].map(([product_id, quantity]) => ({ product_id, quantity }));
    const response = await fetch("/api/checkout", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ items, correction_adjustment_id: state.correctionAdjustmentID }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "会計を確定できませんでした");
    state.quantities.clear();
	state.correctionAdjustmentID = null;
	state.correctionOriginalSaleID = null;
    await refreshInventory();
	loadSales();
    showNotice(`会計 #${result.sale_id} を確定しました。合計 ${amount(result.total)}`);
  } catch (error) {
    showNotice(error.message || "会計を確定できませんでした", true);
  } finally {
    state.busy = false;
    renderProducts();
    renderCart();
  }
});

async function refreshInventory() {
  const response = await fetch("/api/inventory");
  if (!response.ok) throw new Error("在庫情報の読み込みに失敗しました");
  const inventory = await response.json();
  state.inventory = new Map(inventory.items.map((item) => [item.product_id, item.quantity]));
}

function renderSales(sales) {
  if (sales.length === 0) {
    salesList.innerHTML = '<p class="empty-sales">売上はまだありません</p>';
    return;
  }
  salesList.replaceChildren(...sales.map((sale) => {
    const entry = document.createElement("article");
    entry.className = "sale-entry";
    const appliedSets = sale.applied_sets
      ? `<p class="sale-entry__sets">セット: ${escapeHTML(sale.applied_sets)}</p>`
      : "";
	const adjustment = renderAdjustmentStatus(sale);
    entry.innerHTML = `
      <div class="sale-entry__top"><span class="sale-entry__id">会計 #${sale.sale_id}</span><span class="sale-entry__total">${amount(sale.total)}</span></div>
      <p class="sale-entry__date">${escapeHTML(formatSaleTime(sale.created_at))}</p>
      <p class="sale-entry__items">${escapeHTML(sale.item_summary)}</p>
      ${appliedSets}
      ${adjustment}`;
    return entry;
  }));
}

function renderAdjustmentStatus(sale) {
  if (!sale.adjustment_id) {
    return `<button class="sale-adjust-button" type="button" data-action="adjust-sale" data-sale-id="${sale.sale_id}">取消・訂正</button>`;
  }
  const labels = { void: "取消済み", refund: "返品・返金済み", correction: "訂正" };
  const label = labels[sale.adjustment_type] || "処理済み";
  if (sale.adjustment_type === "correction" && sale.adjustment_status === "pending") {
    return `<p class="sale-entry__status is-pending">${label}会計の登録待ち</p><button class="sale-adjust-button" type="button" data-action="resume-correction" data-sale-id="${sale.sale_id}">訂正を再開</button>`;
  }
  const replacement = sale.replacement_sale_id ? ` → 会計 #${sale.replacement_sale_id}` : "";
  return `<p class="sale-entry__status">${label}${replacement}</p>`;
}

salesList.addEventListener("click", (event) => {
  const button = event.target.closest("[data-action]");
  if (!button) return;
  const saleID = Number(button.dataset.saleId);
  const sale = state.lastSales.find((entry) => entry.sale_id === saleID);
  if (!sale) return;
  if (button.dataset.action === "adjust-sale") openAdjustmentDialog(sale);
  if (button.dataset.action === "resume-correction") resumeCorrection(sale);
});

function openAdjustmentDialog(sale) {
  state.selectedSale = sale;
  adjustmentSaleSummary.textContent = `会計 #${sale.sale_id}　${formatSaleTime(sale.created_at)}　${sale.item_summary}　${amount(sale.total)}`;
  adjustmentForm.reset();
  adjustmentError.textContent = "";
  updateAdjustmentForm();
  adjustmentDialog.showModal();
}

function selectedAdjustmentType() {
  return adjustmentForm.querySelector('input[name="adjustment-type"]:checked').value;
}

function updateAdjustmentForm() {
  const correction = selectedAdjustmentType() === "correction";
  adjustmentOtherField.hidden = adjustmentReason.value !== "その他";
  adjustmentCartWarning.hidden = !correction || state.quantities.size === 0;
  adjustmentSubmit.textContent = correction ? "取消して再会計へ" : "処理を確定";
}

adjustmentForm.addEventListener("change", updateAdjustmentForm);
adjustmentCancel.addEventListener("click", () => adjustmentDialog.close());

adjustmentForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  if (!state.selectedSale) return;
  const type = selectedAdjustmentType();
  const reason = adjustmentReason.value === "その他" ? adjustmentOther.value.trim() : adjustmentReason.value;
  if (!reason) {
    adjustmentError.textContent = "理由の詳細を入力してください。";
    return;
  }
  adjustmentSubmit.disabled = true;
  adjustmentCancel.disabled = true;
  adjustmentError.textContent = "";
  try {
    const response = await fetch(`/api/sales/${state.selectedSale.sale_id}/adjustments`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ type, reason }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "取消・訂正を保存できませんでした");
    await refreshInventory();
    if (type === "correction") {
      state.quantities = new Map(result.draft_items.map((item) => [item.product_id, item.quantity]));
      state.correctionAdjustmentID = result.adjustment_id;
      state.correctionOriginalSaleID = result.original_sale_id;
      showNotice(`会計 #${result.original_sale_id} を訂正します。内容を確認して会計を確定してください。`);
    } else {
      showNotice(`会計 #${result.original_sale_id} の${type === "void" ? "取消" : "返品・返金"}を記録しました。`);
    }
    adjustmentDialog.close();
    renderProducts();
    renderCart();
    loadSales();
  } catch (error) {
    adjustmentError.textContent = error.message || "取消・訂正を保存できませんでした";
  } finally {
    adjustmentSubmit.disabled = false;
    adjustmentCancel.disabled = false;
  }
});

async function resumeCorrection(sale) {
  if (state.quantities.size > 0) {
    showNotice("未確定のカートを取り消してから、訂正を再開してください。", true);
    return;
  }
  try {
    const response = await fetch(`/api/sales/${sale.sale_id}/adjustments`);
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "訂正会計を再開できませんでした");
    state.quantities = new Map(result.draft_items.map((item) => [item.product_id, item.quantity]));
    state.correctionAdjustmentID = result.adjustment_id;
    state.correctionOriginalSaleID = result.original_sale_id;
    await refreshInventory();
    renderProducts();
    renderCart();
    showNotice(`会計 #${result.original_sale_id} の訂正を再開しました。`);
  } catch (error) {
    showNotice(error.message || "訂正会計を再開できませんでした", true);
  }
}

function formatSaleTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return value;
  return date.toLocaleString("ja-JP", { dateStyle: "short", timeStyle: "short" });
}

async function loadSales() {
  refreshSalesButton.disabled = true;
  try {
    const response = await fetch("/api/sales?limit=10");
    if (!response.ok) throw new Error("売上ログの読み込みに失敗しました");
    const result = await response.json();
    state.lastSales = result.sales;
    renderSales(result.sales);
  } catch (error) {
    salesList.innerHTML = `<p class="empty-sales">${escapeHTML(error.message)}</p>`;
  } finally {
    refreshSalesButton.disabled = false;
  }
}

async function loadProducts() {
  try {
    const response = await fetch("/api/products");
    if (!response.ok) throw new Error("商品情報の読み込みに失敗しました");
    const catalog = await response.json();
    state.products = catalog.products;
    state.setDiscounts = catalog.set_discounts || [];
    await refreshInventory();
    document.querySelector("#store-name").textContent = catalog.store.name || "POS";
    document.title = `${catalog.store.name || "POS"} | POS`;
    renderProducts();
    renderCart();
	loadSales();
  } catch (error) {
    productList.innerHTML = `<p class="empty-cart">${escapeHTML(error.message)}</p>`;
  }
}

/* Inline settings UI moved to settings.js.
function showSettingsStatus(element, message, isError = false) {
  element.textContent = message;
  element.className = isError ? "settings-status is-error" : "settings-status";
}

function normalizeCartForCatalog() {
  const productIDs = new Set(state.products.map((product) => product.id));
  for (const [productID, quantity] of state.quantities) {
    if (!productIDs.has(productID)) {
      state.quantities.delete(productID);
      continue;
    }
    state.quantities.set(productID, Math.min(quantity, getStock(productID)));
  }
  for (const [productID, quantity] of state.quantities) {
    if (quantity <= 0) state.quantities.delete(productID);
  }
}

function resetSettingsProductForm() {
  state.selectedSettingsProductID = null;
  settingsProductForm.reset();
  settingsProductID.readOnly = false;
  settingsProductFormTitle.textContent = "商品を追加";
  deleteProductButton.hidden = true;
  showSettingsStatus(settingsProductStatus, "");
}

function populateSettingsProductForm(product) {
  state.selectedSettingsProductID = product.id;
  settingsProductID.value = product.id;
  settingsProductName.value = product.name;
  settingsProductPrice.value = product.unit_price;
  settingsProductStock.value = product.initial_stock;
  settingsProductID.readOnly = true;
  settingsProductFormTitle.textContent = "商品を編集";
  deleteProductButton.hidden = false;
  showSettingsStatus(settingsProductStatus, "");
}

function renderSettingsProducts() {
  const catalog = state.settingsCatalog;
  if (!catalog) return;
  settingsProductList.replaceChildren(...catalog.products.map((product) => {
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.productId = product.id;
    if (product.id === state.selectedSettingsProductID) button.classList.add("is-selected");
    button.innerHTML = `<strong>${escapeHTML(product.name)}</strong><span>${amount(product.unit_price)} / 在庫 ${product.initial_stock}</span>`;
    return button;
  }));
  const selected = catalog.products.find((product) => product.id === state.selectedSettingsProductID);
  if (selected) populateSettingsProductForm(selected);
  else resetSettingsProductForm();
}

function createSetItemRow(item = {}) {
  const row = document.createElement("div");
  row.className = "set-item-row";
  const productLabel = document.createElement("label");
  productLabel.textContent = "商品";
  const select = document.createElement("select");
  select.className = "set-item-product";
  select.required = true;
  for (const product of state.settingsCatalog.products) {
    const option = document.createElement("option");
    option.value = product.id;
    option.textContent = product.name;
    select.append(option);
  }
  if (item.product_id) select.value = item.product_id;
  productLabel.append(select);
  const quantityLabel = document.createElement("label");
  quantityLabel.textContent = "数量";
  const quantity = document.createElement("input");
  quantity.className = "set-item-quantity";
  quantity.type = "number";
  quantity.min = "1";
  quantity.step = "1";
  quantity.inputMode = "numeric";
  quantity.required = true;
  quantity.value = item.quantity || 1;
  quantityLabel.append(quantity);
  const removeButton = document.createElement("button");
  removeButton.type = "button";
  removeButton.textContent = "削除";
  removeButton.addEventListener("click", () => {
    row.remove();
    renderEmptySetItemsMessage();
  });
  row.append(productLabel, quantityLabel, removeButton);
  return row;
}

function renderEmptySetItemsMessage() {
  const hasItems = settingsSetItems.querySelector(".set-item-row") !== null;
  const existing = settingsSetItems.querySelector(".set-items-empty");
  if (hasItems && existing) existing.remove();
  if (!hasItems && !existing) {
    const empty = document.createElement("p");
    empty.className = "set-items-empty";
    empty.textContent = "対象商品を1件以上追加してください。";
    settingsSetItems.append(empty);
  }
}

function setSettingsSetItems(items) {
  settingsSetItems.replaceChildren(...items.map((item) => createSetItemRow(item)));
  renderEmptySetItemsMessage();
}

function resetSettingsSetForm() {
  state.selectedSettingsSetID = null;
  settingsSetForm.reset();
  settingsSetID.readOnly = false;
  settingsSetFormTitle.textContent = "セットを追加";
  deleteSetButton.hidden = true;
  setSettingsSetItems([]);
  showSettingsStatus(settingsSetStatus, "");
}

function populateSettingsSetForm(discount) {
  state.selectedSettingsSetID = discount.id;
  settingsSetID.value = discount.id;
  settingsSetName.value = discount.name;
  settingsSetPrice.value = discount.set_price;
  settingsSetID.readOnly = true;
  settingsSetFormTitle.textContent = "セットを編集";
  deleteSetButton.hidden = false;
  setSettingsSetItems(discount.items);
  showSettingsStatus(settingsSetStatus, "");
}

function renderSettingsSets() {
  const catalog = state.settingsCatalog;
  if (!catalog) return;
  settingsSetSelect.replaceChildren();
  const newOption = document.createElement("option");
  newOption.value = "";
  newOption.textContent = "新規セットを追加";
  settingsSetSelect.append(newOption);
  for (const discount of catalog.set_discounts || []) {
    const option = document.createElement("option");
    option.value = discount.id;
    option.textContent = discount.name;
    settingsSetSelect.append(option);
  }
  const selected = (catalog.set_discounts || []).find((discount) => discount.id === state.selectedSettingsSetID);
  if (selected) {
    settingsSetSelect.value = selected.id;
    populateSettingsSetForm(selected);
  } else {
    settingsSetSelect.value = "";
    resetSettingsSetForm();
  }
}

function renderSettingsAudit() {
  settingsAuditDirectory.value = state.settingsCatalog?.audit?.output_directory || "";
  showSettingsStatus(settingsAuditStatus, "");
}

async function loadSettingsCatalog() {
  const response = await fetch("/api/settings/catalog");
  const catalog = await response.json();
  if (!response.ok) throw new Error(catalog.error || "設定を読み込めませんでした");
  state.settingsCatalog = catalog;
  renderSettingsProducts();
  renderSettingsSets();
  renderSettingsAudit();
}

async function openSettings() {
  if (state.busy) return;
  openSettingsButton.disabled = true;
  try {
    await loadSettingsCatalog();
    posLayout.hidden = true;
    settingsScreen.hidden = false;
  } catch (error) {
    showNotice(error.message || "設定を読み込めませんでした", true);
  } finally {
    openSettingsButton.disabled = false;
  }
}

async function closeSettings() {
  settingsScreen.hidden = true;
  posLayout.hidden = false;
  try {
    await loadProducts();
    normalizeCartForCatalog();
    renderProducts();
    renderCart();
  } catch (error) {
    showNotice(error.message || "商品情報を更新できませんでした", true);
  }
}

settingsProductList.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-product-id]");
  if (!button || !state.settingsCatalog) return;
  const product = state.settingsCatalog.products.find((item) => item.id === button.dataset.productId);
  if (!product) return;
  populateSettingsProductForm(product);
  renderSettingsProducts();
});

newProductButton.addEventListener("click", resetSettingsProductForm);

settingsProductForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const productID = settingsProductID.value.trim();
  const unitPrice = Number(settingsProductPrice.value);
  const stock = Number(settingsProductStock.value);
  if (!productID || !Number.isInteger(unitPrice) || !Number.isInteger(stock)) {
    showSettingsStatus(settingsProductStatus, "商品ID・単価・在庫数を正しく入力してください。", true);
    return;
  }
  const submitButton = settingsProductForm.querySelector('button[type="submit"]');
  submitButton.disabled = true;
  try {
    const response = await fetch(`/api/settings/products/${encodeURIComponent(productID)}`, {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name: settingsProductName.value, unit_price: unitPrice, stock }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "商品を保存できませんでした");
    state.selectedSettingsProductID = productID;
    await loadSettingsCatalog();
    showSettingsStatus(settingsProductStatus, `${result.name} を保存しました。`);
  } catch (error) {
    showSettingsStatus(settingsProductStatus, error.message || "商品を保存できませんでした", true);
  } finally {
    submitButton.disabled = false;
  }
});

deleteProductButton.addEventListener("click", async () => {
  const productID = state.selectedSettingsProductID;
  if (!productID || !state.settingsCatalog) return;
  const product = state.settingsCatalog.products.find((item) => item.id === productID);
  if (!product || !window.confirm(`${product.name} を商品設定から削除します。過去の会計記録は削除されません。`)) return;
  deleteProductButton.disabled = true;
  try {
    const response = await fetch(`/api/settings/products/${encodeURIComponent(productID)}`, { method: "DELETE" });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "商品を削除できませんでした");
    state.selectedSettingsProductID = null;
    await loadSettingsCatalog();
    showSettingsStatus(settingsProductStatus, result.message || "商品を削除しました。");
  } catch (error) {
    showSettingsStatus(settingsProductStatus, error.message || "商品を削除できませんでした", true);
  } finally {
    deleteProductButton.disabled = false;
  }
});

settingsSetSelect.addEventListener("change", () => {
  if (!state.settingsCatalog) return;
  const discount = (state.settingsCatalog.set_discounts || []).find((item) => item.id === settingsSetSelect.value);
  if (discount) populateSettingsSetForm(discount);
  else resetSettingsSetForm();
});

newSetButton.addEventListener("click", resetSettingsSetForm);

addSetItemButton.addEventListener("click", () => {
  if (!state.settingsCatalog || state.settingsCatalog.products.length === 0) return;
  const empty = settingsSetItems.querySelector(".set-items-empty");
  if (empty) empty.remove();
  settingsSetItems.append(createSetItemRow());
});

function readSettingsSetItems() {
  const rows = [...settingsSetItems.querySelectorAll(".set-item-row")];
  return rows.map((row) => ({
    product_id: row.querySelector(".set-item-product").value,
    quantity: Number(row.querySelector(".set-item-quantity").value),
  }));
}

settingsSetForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const setID = settingsSetID.value.trim();
  const setPrice = Number(settingsSetPrice.value);
  const items = readSettingsSetItems();
  if (!setID || !Number.isInteger(setPrice) || items.length === 0 || items.some((item) => !Number.isInteger(item.quantity) || item.quantity < 1)) {
    showSettingsStatus(settingsSetStatus, "セットID・価格・対象商品と数量を正しく入力してください。", true);
    return;
  }
  const discount = { id: setID, name: settingsSetName.value.trim(), set_price: setPrice, items };
  const setDiscounts = (state.settingsCatalog.set_discounts || []).filter((item) => item.id !== setID);
  setDiscounts.push(discount);
  const submitButton = settingsSetForm.querySelector('button[type="submit"]');
  submitButton.disabled = true;
  try {
    const response = await fetch("/api/settings/set-discounts", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ set_discounts: setDiscounts }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "セット割引を保存できませんでした");
    state.selectedSettingsSetID = setID;
    await loadSettingsCatalog();
    showSettingsStatus(settingsSetStatus, `${discount.name} を保存しました。`);
  } catch (error) {
    showSettingsStatus(settingsSetStatus, error.message || "セット割引を保存できませんでした", true);
  } finally {
    submitButton.disabled = false;
  }
});

deleteSetButton.addEventListener("click", async () => {
  const setID = state.selectedSettingsSetID;
  if (!setID || !state.settingsCatalog) return;
  const discount = (state.settingsCatalog.set_discounts || []).find((item) => item.id === setID);
  if (!discount || !window.confirm(`${discount.name} を削除します。`)) return;
  const setDiscounts = (state.settingsCatalog.set_discounts || []).filter((item) => item.id !== setID);
  deleteSetButton.disabled = true;
  try {
    const response = await fetch("/api/settings/set-discounts", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ set_discounts: setDiscounts }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "セット割引を削除できませんでした");
    state.selectedSettingsSetID = null;
    await loadSettingsCatalog();
    showSettingsStatus(settingsSetStatus, "セット割引を削除しました。");
  } catch (error) {
    showSettingsStatus(settingsSetStatus, error.message || "セット割引を削除できませんでした", true);
  } finally {
    deleteSetButton.disabled = false;
  }
});

async function saveAuditSettings() {
  const response = await fetch("/api/settings/audit", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ output_directory: settingsAuditDirectory.value.trim() }),
  });
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || "監査出力先を保存できませんでした");
  if (state.settingsCatalog) state.settingsCatalog.audit = result;
  return result;
}

settingsAuditForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const submitButton = settingsAuditForm.querySelector('button[type="submit"]');
  submitButton.disabled = true;
  try {
    await saveAuditSettings();
    showSettingsStatus(settingsAuditStatus, "監査CSVの出力先を保存しました。");
  } catch (error) {
    showSettingsStatus(settingsAuditStatus, error.message || "監査出力先を保存できませんでした", true);
  } finally {
    submitButton.disabled = false;
  }
});

exportAuditButton.addEventListener("click", async () => {
  exportAuditButton.disabled = true;
  try {
    if (settingsAuditDirectory.value.trim() !== (state.settingsCatalog?.audit?.output_directory || "")) await saveAuditSettings();
    const response = await fetch("/api/audit/export", { method: "POST" });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "監査CSVを出力できませんでした");
    showSettingsStatus(settingsAuditStatus, `監査CSVを出力しました: ${result.path}`);
  } catch (error) {
    showSettingsStatus(settingsAuditStatus, error.message || "監査CSVを出力できませんでした", true);
  } finally {
    exportAuditButton.disabled = false;
  }
});

openSettingsButton.addEventListener("click", openSettings);
closeSettingsButton.addEventListener("click", closeSettings);
*/

function showEventSelectionError(message) {
  eventSelectionError.textContent = message;
}

async function loadEventList() {
  const response = await fetch("/api/events");
  const result = await response.json();
  if (!response.ok) throw new Error(result.error || "既存のイベントDBを読み込めませんでした");
  if (result.events.length === 0) {
    eventList.innerHTML = '<p class="empty-event-list">既存のイベントDBはありません。</p>';
    return;
  }
  eventList.replaceChildren(...result.events.map((event) => {
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.eventId = event.id;
    button.textContent = event.id === "default" ? "既定の pos.db" : event.id;
    return button;
  }));
}

async function startSelectedEvent(openSettings = false) {
  eventSelectionDialog.close();
  if (openSettings) {
    window.location.assign("/settings.html");
    return;
  }
  posHeader.hidden = false;
  posLayout.hidden = false;
  await loadProducts();
}

async function openExistingEvent(eventID) {
  eventList.querySelectorAll("button").forEach((button) => { button.disabled = true; });
  showEventSelectionError("");
  try {
    const response = await fetch("/api/events/open", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id: eventID }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "イベントDBを開けませんでした");
    await startSelectedEvent();
  } catch (error) {
    showEventSelectionError(error.message || "イベントDBを開けませんでした");
    eventList.querySelectorAll("button").forEach((button) => { button.disabled = false; });
  }
}

eventList.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-event-id]");
  if (button) openExistingEvent(button.dataset.eventId);
});

eventCreateForm.addEventListener("submit", async (event) => {
  event.preventDefault();
  const id = eventCreateID.value.trim();
  const submitButton = eventCreateForm.querySelector('button[type="submit"]');
  if (!id) return;
  submitButton.disabled = true;
  showEventSelectionError("");
  try {
    const response = await fetch("/api/events/create", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ id }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "イベントDBを作成できませんでした");
    await startSelectedEvent(true);
  } catch (error) {
    showEventSelectionError(error.message || "イベントDBを作成できませんでした");
    submitButton.disabled = false;
  }
});

eventSelectionDialog.addEventListener("cancel", (event) => event.preventDefault());

async function initializePOS() {
  try {
    const response = await fetch("/api/events/selection");
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "POSの起動状態を確認できませんでした");
    if (result.selected) {
      posHeader.hidden = false;
      posLayout.hidden = false;
      await loadProducts();
      return;
    }
    await loadEventList();
    eventSelectionDialog.showModal();
  } catch (error) {
    posHeader.hidden = false;
    posLayout.hidden = false;
    productList.innerHTML = `<p class="empty-cart">${escapeHTML(error.message || "POSを初期化できませんでした")}</p>`;
  }
}

initializePOS();
