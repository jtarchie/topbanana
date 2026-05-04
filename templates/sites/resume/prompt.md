---
{
  "label": "Resume / personal site",
  "description": "One-page personal résumé with about, experience, skills, and contact.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "résumé pages need a clear <h1> with the person's name"
    }
  ]
}
---
Site type: personal résumé.

- index.html is a single-page résumé. Inline CSS only.
- Sections in this order: name + tagline header, About, Experience, Skills, Contact.
- Experience entries should include role, company, dates, and one or two bullet points.
- Keep typography clean and readable. Black on off-white, generous line height. No images required.
- Replace every placeholder with content the user described.
