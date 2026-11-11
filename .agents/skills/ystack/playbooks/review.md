# Review playbook

Review code, a design or a plan and recommend improvements. Stay in stages 1 and 2 unless asked.

1. Read the project's principles and the guide for the area under review.
2. Map the area: size per file, main types, who owns which fact.
3. Stage 1: for each part, who needs it and what evidence says so. Flag unowned requirements.
4. Stage 2: find what can be deleted outright (decided-against paths, leftovers, duplicate copies,
   prose that restates code, unused options).
5. List stage 3 candidates as next steps, such as repeated shapes that hide one domain concept
   (three copies of one flow).
6. Rank by payoff over cost. Order so each recommendation makes the next one smaller.
7. For each: evidence with file:line, the change, what it deletes, what it costs, how to verify.

Reply: at most 3 to 5 recommendations, deletion first, each with evidence and payoff. Offer to start
with the cheapest one.
