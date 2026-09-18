-- Application policy has moved to clients. Retain generic execution custody.
drop view dorf.review_run_projection;
drop table dorf.job_outcomes;
drop table dorf.github_proposals;
drop table dorf.review_plans;
drop table dorf.revisions;
drop table dorf.evidence;
drop table dorf.coding_to_proposal_inputs;

insert into dorf.schema_migrations(name) values ('023_remove_coding.sql');
