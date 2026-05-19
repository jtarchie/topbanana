// Edit this file to define your products. The agent will rewrite it based on
// what the user said they're selling. Keep ids URL-safe (a-z, 0-9, hyphen).
//
// Optional: set buy_button_id to a Stripe Buy Button ID (buy_btn_...) to make
// that product check out via Stripe instead of the in-house order form. The
// publishable key for ALL Stripe Buy Buttons on this site lives in index.html
// as STRIPE_PUBLISHABLE_KEY.
var PRODUCTS = [
  { id: "alpha", name: "Alpha", price: "$10", description: "Replace this with a real description." },
  { id: "beta",  name: "Beta",  price: "$15", description: "Replace this with a real description." },
];

module.exports = function () {
  return response.json(PRODUCTS);
};
