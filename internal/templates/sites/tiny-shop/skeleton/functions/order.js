// Mirror of PRODUCTS in products.js. Keep in sync — duplicated so the order
// validator doesn't need to import or rebuild a runtime catalog from kv.
var PRODUCTS = [
  { id: "alpha", name: "Alpha", price: "$10" },
  { id: "beta",  name: "Beta",  price: "$15" },
];

module.exports = function (request) {
  var form = request.form || {};
  var product = PRODUCTS.find(function (p) { return p.id === form.product; });
  if (!product) {
    return response.status(400, "unknown product");
  }
  var qty = parseInt(form.qty || "1", 10);
  if (!qty || qty < 1 || qty > 99) {
    return response.status(400, "qty must be 1-99");
  }
  if (!form.name || !form.email) {
    return response.status(400, "name and email are required");
  }
  var seq = kv.incr("order_seq");
  kv.put("order:" + String(seq).padStart(8, "0"), {
    product: product.id,
    product_name: product.name,
    qty: qty,
    name: String(form.name).slice(0, 80),
    email: String(form.email).slice(0, 120),
    ts: Date.now()
  });
  console.log("order", seq, product.id, qty);
  return response.redirect("/thanks.html");
};
