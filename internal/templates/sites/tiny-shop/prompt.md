---
{
  "label": "Tiny shop",
  "description": "One-page product list with an order form. Orders persist; the owner can see them. No payments.",
  "enables_functions": true,
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<form", "/api/order"],
      "message": "tiny-shop sites need an order <form> that posts to /api/order"
    }
  ]
}
---
Site type: tiny shop / preorder list / single-vendor stall.

- This template ships a product list (in functions/products.js), a customer-facing order page (index.html), and a tiny order-log page (orders.html) the owner uses to see what was sold. No payments.
- Edit functions/products.js to define the products you're selling. Each entry: { id, name, price (string for display), description }.
- The order form on index.html posts name/email/product/qty to /api/order. functions/order.js validates against the product list and persists `order:NNNNNNNN` entries.
- orders.html fetches /api/orders for the owner to scan. Add lightweight auth (this site's basic-auth covers it) before the kv data leaks to anyone.
- Inline CSS. Make products feel like products — image (optional, via list_assets), name, price, short description, "order" button anchoring to the form.
