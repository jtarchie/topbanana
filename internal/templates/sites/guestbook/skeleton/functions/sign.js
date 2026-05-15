module.exports = function (request) {
  var result = validate(request.form, {
    name:    { type: "string", required: true, maxLen: 60,   trim: true },
    message: { type: "string", required: true, maxLen: 1000, trim: true },
  });
  if (!result.ok) {
    return response.json({ errors: result.errors }, 400);
  }
  var seq = kv.incr("seq");
  var key = "entry:" + String(seq).padStart(8, "0");
  kv.put(key, {
    name: result.data.name,
    message: result.data.message,
    ts: Date.now()
  });
  return response.redirect("/");
};
