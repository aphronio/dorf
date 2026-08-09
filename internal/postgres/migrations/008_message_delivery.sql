alter table dorf.job_messages
    add column delivery_intent text not null default 'follow',
    add column steer_target_turn_id text;

alter table dorf.job_messages
    add constraint job_messages_delivery_intent_check
        check (delivery_intent in ('follow','steer')),
    add constraint job_messages_delivery_target_check
        check ((delivery_intent='follow' and steer_target_turn_id is null) or
               (delivery_intent='steer' and steer_target_turn_id is not null));

alter table dorf.agent_runs drop constraint if exists agent_runs_native_turn_id_key;

comment on table dorf.job_messages is 'Immutable Dorf-owned client input and monotonic admission sequence; follow turns are FIFO while steer targets the active turn through an explicit priority lane';
comment on table dorf.agent_runs is 'Per-input harness acceptance binding and native outcome evidence only; Codex owns transcript and context';
