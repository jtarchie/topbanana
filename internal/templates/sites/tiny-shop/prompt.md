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
  ],
  "setup_notes": "By default, this template records orders in an in-house log — no payments are collected. You can opt any product into Stripe Buy Button checkout instead.\n\nTo enable Stripe for a product:\n1. Create a Buy Button at https://dashboard.stripe.com/buy-buttons\n2. In the workspace, open functions/products.js and add `buy_button_id: \"buy_btn_...\"` to that product entry.\n3. Open index.html and replace `pk_live_REPLACE_ME` with your Stripe publishable key (one key covers all Buy Buttons on the site).\n\nProducts without a buy_button_id keep using the in-house order form; visit /orders.html on your site to see those orders. If every product has a buy_button_id, the order form hides itself automatically."
}
---
Site type: tiny shop / preorder list / single-vendor stall.

- This template ships a product list (in functions/products.js), a customer-facing order page (index.html), and a tiny order-log page (orders.html) the owner uses to see what was sold. No payments.
- Edit functions/products.js to define the products you're selling. Each entry: { id, name, price (string for display), description }.
- The order form on index.html posts name/email/product/qty to /api/order. functions/order.js validates against the product list and persists `order:NNNNNNNN` entries.
- orders.html fetches /api/orders for the owner to scan. Add lightweight auth (this site's basic-auth covers it) before the kv data leaks to anyone.
- Inline CSS. Make products feel like products — image (optional, via list_assets), name, price, short description, "order" button anchoring to the form.

Stripe Buy Button integration (optional, per product):

- Each entry in functions/products.js accepts an optional `buy_button_id: "buy_btn_..."` field. When set, that product card renders a `<stripe-buy-button>` (Stripe Checkout opens in an overlay) and is excluded from the in-house order form's `<select>`. Leave the field off (or `null`) for products that should keep using the kv order log.
- Site-wide publishable key: index.html declares `var STRIPE_PUBLISHABLE_KEY = "pk_live_REPLACE_ME"`. When the user gives you their Stripe publishable key, replace that string. One publishable key works for any number of Buy Buttons on the page.
- Use `buy_btn_REPLACE_ME` as the placeholder for buy_button_id values you don't know yet. Never invent realistic-looking Stripe IDs.
- Both flows can coexist on the same site: e.g., a "t-shirt" product can have a Buy Button while a "preorder waitlist" product uses the kv form. If every product has a buy_button_id, the order form hides itself automatically.
