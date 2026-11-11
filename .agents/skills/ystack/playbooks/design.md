# Design playbook

A view, page or visual style for the owner. Whole-page rebuilds get rejected; swatch rounds converge
in an afternoon.

1. Name the questions the view answers, in the owner's words ("where is the bottleneck, is momentum
   up, what is on me"). Every element answers one; delete the rest, including parked work.
2. Probe style before building: 6 to 9 distinct directions of one small component, inline
   (show_widget), with tick-and-send picks and a note field. Never rebuild the app to test a taste.
   Ask for 2 or 3 screenshots of things they like; that is the fastest signal.
3. Narrow one axis per round: style, then structure, then colour, then words. Keep what they ticked,
   change one thing, show it again.
4. Use live numbers from the source of truth in every probe, never lorem. Real data exposes unclear
   labels and empty rows that placeholders hide.
5. Norman test on every label: if it needs explaining ("reached", "waiting", "with a signal"), the
   design is wrong. Rename it in their words or delete it; never add help text. Ask "do I need this?"
6. Colour is meaning, written as a legend of meanings before any hex: the number to read, structure,
   too early, act, worse. At most two accents in view; drop a colour when order or fade says it.
7. Conclusion first: each section heading carries a one-line read of its numbers ("twice the sends,
   three times the positive replies"), computed in code, never written by hand.
8. Dense beats decorated: tables, one series per row, values printed. No dual axes, no legends to
   decode. Sort by result so position shows what works.
9. Split glance from action: the glance shows state and links to the page where each action lives.
10. Design for the data that exists or can exist; do not let today's data shape shrink the view.
    List the data fix (a field, a backfill, a classifier) as its own step.
11. Build once, after picks converge: rules and reads in code, the page only draws. Write the
    project's DESIGN.md: the language, what was rejected and why, open items.

Reply: the picks so far, the one axis this round changes, the swatches. After the build: a screenshot,
what changed from the last approved probe, LOC delta.
