const state = {
  catalog: null,
  selectedProductID: null,
  selectedSetID: null,
};

const settingsScreen = document.querySelector("#settings-screen");
const settingsStoreName = document.querySelector("#settings-store-name");
const settingsPageError = document.querySelector("#settings-page-error");
const settingsProductList = document.querySelector("#settings-product-list");
const settingsProductForm = document.querySelector("#settings-product-form");
const settingsProductID = document.querySelector("#settings-product-id");
const settingsProductName = document.querySelector("#settings-product-name");
const settingsProductPrice = document.querySelector("#settings-product-price");
const settingsProductStock = document.querySelector("#settings-product-stock");
const settingsProductFormTitle = document.querySelector("#settings-product-form-title");
const settingsProductStatus = document.querySelector("#settings-product-status");
const newProductButton = document.querySelector("#new-product");
const deleteProductButton = document.querySelector("#delete-product");
const settingsSetSelect = document.querySelector("#settings-set-select");
const settingsSetForm = document.querySelector("#settings-set-form");
const settingsSetID = document.querySelector("#settings-set-id");
const settingsSetName = document.querySelector("#settings-set-name");
const settingsSetPrice = document.querySelector("#settings-set-price");
const settingsSetItems = document.querySelector("#settings-set-items");
const settingsSetFormTitle = document.querySelector("#settings-set-form-title");
const settingsSetStatus = document.querySelector("#settings-set-status");
const newSetButton = document.querySelector("#new-set");
const addSetItemButton = document.querySelector("#add-set-item");
const deleteSetButton = document.querySelector("#delete-set");
const settingsAuditForm = document.querySelector("#settings-audit-form");
const settingsAuditDirectory = document.querySelector("#settings-audit-directory");
const settingsAuditStatus = document.querySelector("#settings-audit-status");
const exportAuditButton = document.querySelector("#export-audit");
const exportOperationsAuditButton = document.querySelector("#export-operations-audit");

const yen = new Intl.NumberFormat("ja-JP", { style: "currency", currency: "JPY", maximumFractionDigits: 0 });
const amount = (value) => yen.format(value);

function escapeHTML(value) {
  const element = document.createElement("span");
  element.textContent = value;
  return element.innerHTML;
}

function showSettingsStatus(element, message, isError = false) {
  element.textContent = message;
  element.className = isError ? "settings-status is-error" : "settings-status";
}

function resetSettingsProductForm() {
  state.selectedProductID = null;
  settingsProductForm.reset();
  settingsProductID.readOnly = false;
  settingsProductFormTitle.textContent = "商品を追加";
  deleteProductButton.hidden = true;
  showSettingsStatus(settingsProductStatus, "");
}

function populateSettingsProductForm(product) {
  state.selectedProductID = product.id;
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
  if (!state.catalog) return;
  settingsProductList.replaceChildren(...state.catalog.products.map((product) => {
    const button = document.createElement("button");
    button.type = "button";
    button.dataset.productId = product.id;
    if (product.id === state.selectedProductID) button.classList.add("is-selected");
    button.innerHTML = `<strong>${escapeHTML(product.name)}</strong><span>${amount(product.unit_price)} / 在庫 ${product.initial_stock}</span>`;
    return button;
  }));
  const selected = state.catalog.products.find((product) => product.id === state.selectedProductID);
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
  for (const product of state.catalog.products) {
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
  state.selectedSetID = null;
  settingsSetForm.reset();
  settingsSetID.readOnly = false;
  settingsSetFormTitle.textContent = "セットを追加";
  deleteSetButton.hidden = true;
  setSettingsSetItems([]);
  showSettingsStatus(settingsSetStatus, "");
}

function populateSettingsSetForm(discount) {
  state.selectedSetID = discount.id;
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
  if (!state.catalog) return;
  settingsSetSelect.replaceChildren();
  const newOption = document.createElement("option");
  newOption.value = "";
  newOption.textContent = "新規セットを追加";
  settingsSetSelect.append(newOption);
  for (const discount of state.catalog.set_discounts || []) {
    const option = document.createElement("option");
    option.value = discount.id;
    option.textContent = discount.name;
    settingsSetSelect.append(option);
  }
  const selected = (state.catalog.set_discounts || []).find((discount) => discount.id === state.selectedSetID);
  if (selected) {
    settingsSetSelect.value = selected.id;
    populateSettingsSetForm(selected);
  } else {
    settingsSetSelect.value = "";
    resetSettingsSetForm();
  }
}

function renderSettingsAudit() {
  settingsAuditDirectory.value = state.catalog?.audit?.output_directory || "";
  showSettingsStatus(settingsAuditStatus, "");
}

async function loadSettingsCatalog() {
  const response = await fetch("/api/settings/catalog");
  const catalog = await response.json();
  if (!response.ok) throw new Error(catalog.error || "設定を読み込めませんでした");
  state.catalog = catalog;
  settingsStoreName.textContent = catalog.store.name || "POS";
  document.title = `${catalog.store.name || "POS"} | 設定`;
  renderSettingsProducts();
  renderSettingsSets();
  renderSettingsAudit();
}

settingsProductList.addEventListener("click", (event) => {
  const button = event.target.closest("button[data-product-id]");
  if (!button || !state.catalog) return;
  const product = state.catalog.products.find((item) => item.id === button.dataset.productId);
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
    state.selectedProductID = productID;
    await loadSettingsCatalog();
    showSettingsStatus(settingsProductStatus, `${result.name} を保存しました。`);
  } catch (error) {
    showSettingsStatus(settingsProductStatus, error.message || "商品を保存できませんでした", true);
  } finally {
    submitButton.disabled = false;
  }
});

deleteProductButton.addEventListener("click", async () => {
  const productID = state.selectedProductID;
  if (!productID || !state.catalog) return;
  const product = state.catalog.products.find((item) => item.id === productID);
  if (!product || !window.confirm(`${product.name} を商品設定から削除します。過去の会計記録は削除されません。`)) return;
  deleteProductButton.disabled = true;
  try {
    const response = await fetch(`/api/settings/products/${encodeURIComponent(productID)}`, { method: "DELETE" });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "商品を削除できませんでした");
    state.selectedProductID = null;
    await loadSettingsCatalog();
    showSettingsStatus(settingsProductStatus, result.message || "商品を削除しました。");
  } catch (error) {
    showSettingsStatus(settingsProductStatus, error.message || "商品を削除できませんでした", true);
  } finally {
    deleteProductButton.disabled = false;
  }
});

settingsSetSelect.addEventListener("change", () => {
  if (!state.catalog) return;
  const discount = (state.catalog.set_discounts || []).find((item) => item.id === settingsSetSelect.value);
  if (discount) populateSettingsSetForm(discount);
  else resetSettingsSetForm();
});

newSetButton.addEventListener("click", resetSettingsSetForm);

addSetItemButton.addEventListener("click", () => {
  if (!state.catalog || state.catalog.products.length === 0) return;
  settingsSetItems.querySelector(".set-items-empty")?.remove();
  settingsSetItems.append(createSetItemRow());
});

function readSettingsSetItems() {
  return [...settingsSetItems.querySelectorAll(".set-item-row")].map((row) => ({
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
  const setDiscounts = (state.catalog.set_discounts || []).filter((item) => item.id !== setID);
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
    state.selectedSetID = setID;
    await loadSettingsCatalog();
    showSettingsStatus(settingsSetStatus, `${discount.name} を保存しました。`);
  } catch (error) {
    showSettingsStatus(settingsSetStatus, error.message || "セット割引を保存できませんでした", true);
  } finally {
    submitButton.disabled = false;
  }
});

deleteSetButton.addEventListener("click", async () => {
  const setID = state.selectedSetID;
  if (!setID || !state.catalog) return;
  const discount = (state.catalog.set_discounts || []).find((item) => item.id === setID);
  if (!discount || !window.confirm(`${discount.name} を削除します。`)) return;
  const setDiscounts = (state.catalog.set_discounts || []).filter((item) => item.id !== setID);
  deleteSetButton.disabled = true;
  try {
    const response = await fetch("/api/settings/set-discounts", {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ set_discounts: setDiscounts }),
    });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "セット割引を削除できませんでした");
    state.selectedSetID = null;
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
  state.catalog.audit = result;
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
    if (settingsAuditDirectory.value.trim() !== (state.catalog?.audit?.output_directory || "")) await saveAuditSettings();
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

exportOperationsAuditButton.addEventListener("click", async () => {
  exportOperationsAuditButton.disabled = true;
  try {
    if (settingsAuditDirectory.value.trim() !== (state.catalog?.audit?.output_directory || "")) await saveAuditSettings();
    const response = await fetch("/api/audit/operations/export", { method: "POST" });
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "運用監査CSVを出力できませんでした");
    showSettingsStatus(settingsAuditStatus, `運用監査CSVを出力しました: ${result.path}`);
  } catch (error) {
    showSettingsStatus(settingsAuditStatus, error.message || "運用監査CSVを出力できませんでした", true);
  } finally {
    exportOperationsAuditButton.disabled = false;
  }
});

async function initializeSettings() {
  try {
    const response = await fetch("/api/events/selection");
    const result = await response.json();
    if (!response.ok) throw new Error(result.error || "POSの起動状態を確認できませんでした");
    if (!result.selected) throw new Error("先にPOS画面でイベントDBを選択してください。");
    await loadSettingsCatalog();
  } catch (error) {
    settingsScreen.hidden = true;
    settingsPageError.hidden = false;
    settingsPageError.textContent = error.message || "設定を読み込めませんでした";
  }
}

initializeSettings();
