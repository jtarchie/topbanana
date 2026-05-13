module.exports = function (request) {
  var form = request.form || {};
  if (!form.name || !form.message) {
    return response.status(400, "name and message are required");
  }
  var seq = kv.incr("seq");
  var key = "entry:" + String(seq).padStart(8, "0");
  kv.put(key, {
    name: String(form.name).slice(0, 60),
    message: String(form.message).slice(0, 1000),
    ts: Date.now()
  });
  return response.redirect("/");
};
