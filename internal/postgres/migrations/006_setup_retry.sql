alter table dorf.jobs
    add column if not exists setup_action_id text references dorf.actions(id);

update dorf.jobs j
   set setup_action_id = a.id
  from dorf.actions a
 where j.setup_action_id is null
   and a.job_id = j.id
   and a.kind = 'repository-setup'
   and a.message_id is null
   and a.scope_key = '';

comment on column dorf.jobs.setup_action_id is
    'Current repository setup generation; terminal failed Actions and Evidence remain historical';
