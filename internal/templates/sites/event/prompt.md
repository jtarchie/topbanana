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
  ]
}
---
Site type: event invitation.

- index.html announces an event. Inline CSS only.
- Include: event title (`<h1>`), a short tagline or theme line, the date and time, the location (or "online" + link), a short description paragraph, and an RSVP call to action.
- The RSVP can be a `mailto:` link, a phone number, or a `<form>` — pick whatever fits the user's prompt.
- Style should match the event's vibe: formal for weddings, playful for casual meetups. Default to a warm, neutral palette if the user doesn't specify.
- Replace every placeholder.
