// Mirror of PRODUCTS in products.js. Keep in sync — duplicated so the order
// validator doesn't need to import or rebuild a runtime catalog from kv.
var PRODUCTS = [
  { id: "alpha", name: "Alpha", price: "$10" },
  { id: "beta",  name: "Beta",  price: "$15" },
];

module.exports = function (request) {
  var result = validate(request.form, {
    product: { type: "string",  required: true, maxLen: 40, trim: true },
    qty:     { type: "integer", required: true, min: 1, max: 99 },
    name:    { type: "string",  required: true, maxLen: 80,  trim: true },
    email:   { type: "email",   required: true, maxLen: 120, trim: true },
  });
  if (!result.ok) {
    return response.json({ errors: result.errors }, 400);
  }
  var product = PRODUCTS.find(function (p) { return p.id === result.data.product; });
  if (!product) {
    return response.json({ errors: [{ field: "product", message: "unknown product" }] }, 400);
  }
  var seq = kv.incr("order_seq");
  kv.put("order:" + String(seq).padStart(8, "0"), {
    product: product.id,
    product_name: product.name,
    qty: result.data.qty,
    name: result.data.name,
    email: result.data.email,
    ts: Date.now()
  });
  console.log("order", seq, product.id, result.data.qty);
  return response.redirect("/thanks.html");
};
