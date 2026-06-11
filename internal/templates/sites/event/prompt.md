---
{
  "label": "Event invite",
  "description": "Generic event invitation: meetup, wedding, conference, dinner — date, place, RSVP.",
  "checks": [
    {
      "file": "index.html",
      "must_contain": ["<h1"],
      "message": "event pages need a clear <h1> with the event title"
    }
  ],
  "guide": [
    {
      "id": "datetime",
      "label": "Date and time",
      "why": "The date and time decide whether people can come.",
      "how": "Add a 'When' section stating the date and start time clearly.",
      "detector": "section_present",
      "params": { "keywords": ["when", "date", "time", "schedule"] }
    },
    {
      "id": "location",
      "label": "Where it's happening",
      "why": "Guests need the venue — or 'online' plus a join link.",
      "how": "Add a 'Where' section with the venue address or a join link.",
      "detector": "section_present",
      "params": { "keywords": ["where", "location", "venue", "address", "online"] }
    },
    {
      "id": "rsvp",
      "label": "A way to RSVP",
      "why": "An RSVP lets you plan numbers.",
      "how": "Add an RSVP form so guests can respond.",
      "detector": "form"
    },
    {
      "id": "details",
      "label": "A short description",
      "why": "A sentence or two sets expectations for the event.",
      "how": "Add a short 'About' or 'Details' section.",
      "detector": "section_present",
      "params": { "keywords": ["about", "details", "what to expect"] },
      "required": false
    }
  ]
}
---
Site type: event invitation.

- index.html announces an event. Inline CSS only.
- Include: event title (`<h1>`), a short tagline or theme line, the date and time, the location (or "online" + link), a short description paragraph, and an RSVP call to action.
- The RSVP can be a `mailto:` link, a phone number, or a `<form>` — pick whatever fits the user's prompt.
- Style should match the event's vibe: formal for weddings, playful for casual meetups. Default to a warm, neutral palette if the user doesn't specify.
- Replace every placeholder.
