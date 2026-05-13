// Edit this file to define your products. The agent will rewrite it based on
// what the user said they're selling. Keep ids URL-safe (a-z, 0-9, hyphen).
var PRODUCTS = [
  { id: "alpha", name: "Alpha", price: "$10", description: "Replace this with a real description." },
  { id: "beta",  name: "Beta",  price: "$15", description: "Replace this with a real description." },
];

module.exports = function () {
  return response.json(PRODUCTS);
};
