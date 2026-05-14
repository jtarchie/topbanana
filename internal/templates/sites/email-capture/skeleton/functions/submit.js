module.exports = function (request) {
  var form = request.form || {};
  if (!form.email) {
    return response.status(400, "email is required");
  }
  var seq = kv.incr("submission_seq");
  kv.put("submission:" + String(seq).padStart(8, "0"), {
    email: String(form.email).slice(0, 200),
    ts: Date.now()
  });
  return response.redirect("/thanks.html");
};
