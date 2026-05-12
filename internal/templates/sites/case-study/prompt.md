---
{
  "label": "Case study",
  "description": "Long-form customer story: problem, solution, and measurable results.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "case studies need an <h1> with the customer name or headline"
    }
  ]
}
---
Site type: case study.

- index.html tells a single customer's story in long form. Inline CSS only.
- Sections in order: kicker (e.g. "CASE STUDY"), `<h1>` headline featuring the customer, a one-line summary, 3 result stat cards (number + label), Problem section, Solution section, Results section, and a closing pull quote.
- Stat cards should show concrete numbers ("3× faster", "50% lower cost") — replace the placeholders with whatever the user describes.
- Editorial typography. Limit body width for readability (~36rem).
