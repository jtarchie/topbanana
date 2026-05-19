# Tiny shop

## Purpose
A one-page product list with an order form. Orders persist to kv; the owner views them at `/orders.html`. Picked for single-vendor stalls, preorder lists, "I'm selling a thing" pages — no real payments by default, with Stripe Buy Buttons as an opt-in per product.

## What ships
- `skeleton/index.html` — page with product cards and `<form action="/api/order">`.
- `skeleton/thanks.html` — post-order confirmation page.
- `skeleton/orders.html` — owner-facing order log, fetches `/api/orders`.
- `skeleton/functions/products.js` — defines the product list.
- `skeleton/functions/order.js` — validates and persists orders.
- `skeleton/functions/orders.js` — returns all persisted orders for `orders.html`.

## Checks
- `index.html` must contain `<form` and `/api/order` — the order form is the template's contract.

## Config
- `enables_functions: true` — required for the `/api/order|orders|products` endpoints.
- **Optional Stripe Buy Button per product**: each product in `products.js` accepts an optional `buy_button_id`. When set, that product card renders a `<stripe-buy-button>` and is excluded from the in-house order form's `<select>`. `index.html` declares a site-wide `STRIPE_PUBLISHABLE_KEY` constant for all Buy Buttons.
- `setup_notes` surfaces the Stripe steps on the manage page.

## Gotchas
The Stripe and in-house flows coexist: kv-only products and Buy-Button products can live on the same page. The order form auto-hides via JS if every product has a `buy_button_id`. The lint check still passes when the form is hidden because the `<form action="/api/order">` element stays in the DOM.

If a visitor forges a POST to `/api/order` for a Stripe-only product, the kv log records the order without a payment — same risk profile as the original "no payments, just intent capture" framing of this template.
