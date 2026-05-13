module.exports = function () {
  // kv.list returns key-sorted entries; submission keys are zero-padded so
  // the natural sort matches insertion order.
  var rows = kv.list("entry:");
  return response.json(rows.map(function (r) { return r.value; }));
};
