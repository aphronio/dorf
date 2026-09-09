alter table dorf.agent_runs add column interrupt_requested boolean not null default false;
alter table dorf.agent_runs add constraint interrupt_requires_bound_turn
    check (not interrupt_requested or (harness is not null and thread_id is not null and turn_id is not null));

alter table dorf.job_messages add column requested_intent text not null default 'follow'
    check (requested_intent in ('auto','follow','steer'));
update dorf.job_messages set requested_intent=delivery_intent;

insert into dorf.schema_migrations(name) values ('003_message_interrupt.sql');
