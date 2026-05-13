module.exports = function () {
  // Live counter shown on index.html. We use the submission_seq counter
  // maintained by submit.js — it survives restarts because state is durable.
  return response.json({ count: kv.get("submission_seq", 0) });
};
