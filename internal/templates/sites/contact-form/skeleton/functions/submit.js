module.exports = function (request) {
  // Tailor field names to whatever the form is collecting; this skeleton
  // assumes name + email + message. The agent should add or remove fields to
  // match the HTML form, keeping the validate() schema in sync.
  var result = validate(request.form, {
    name:    { type: "string", required: true, maxLen: 80,   trim: true },
    email:   { type: "email",  required: true, maxLen: 120,  trim: true },
    message: { type: "string", required: true, maxLen: 2000, trim: true },
  });
  if (!result.ok) {
    return response.json({ errors: result.errors }, 400);
  }
  var seq = kv.incr("submission_seq");
  kv.put("submission:" + String(seq).padStart(8, "0"),
    Object.assign({ ts: Date.now() }, result.data));
  console.log("submission", seq);
  return response.redirect("/thanks.html");
};
