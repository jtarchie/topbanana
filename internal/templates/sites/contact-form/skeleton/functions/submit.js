module.exports = function (request) {
  // request.form is pre-parsed from application/x-www-form-urlencoded bodies.
  // For Phase 1, just log the submission. Persistence lands in Phase 2 (kv).
  console.log("submission", JSON.stringify(request.form || {}));
  return response.redirect("/thanks.html");
};
