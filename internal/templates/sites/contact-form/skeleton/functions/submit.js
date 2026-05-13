module.exports = function (request) {
  // request.form is pre-parsed from application/x-www-form-urlencoded bodies.
  // Each submission gets a monotonic key so list() returns them in insertion
  // order. Replace the body for your needs — the agent should tailor field
  // names to whatever the user is collecting.
  var seq = kv.incr("submission_seq");
  kv.put("submission:" + String(seq).padStart(8, "0"), request.form || {});
  console.log("submission", seq, JSON.stringify(request.form || {}));
  return response.redirect("/thanks.html");
};
