module.exports = function () {
  var rows = kv.list("order:");
  return response.json(rows.map(function (r) { return r.value; }));
};
