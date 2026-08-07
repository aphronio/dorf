alter table dorf.jobs
    add column if not exists run_terminal_state text,
    add column if not exists run_terminal_at timestamptz;

do $$
begin
    if not exists (
        select 1 from pg_constraint
        where conrelid = 'dorf.jobs'::regclass
          and conname = 'jobs_run_terminal_state_check'
    ) then
        alter table dorf.jobs add constraint jobs_run_terminal_state_check
            check (run_terminal_state in ('failed', 'cancelled'));
    end if;
    if not exists (
        select 1 from pg_constraint
        where conrelid = 'dorf.jobs'::regclass
          and conname = 'jobs_revision_full_oid_check'
    ) then
        -- Existing disposable development rows may predate strict admission. PostgreSQL
        -- still enforces a NOT VALID check for every new or changed row.
        alter table dorf.jobs add constraint jobs_revision_full_oid_check
            check (revision ~ '^[0-9a-f]{40}([0-9a-f]{24})?$') not valid;
    end if;
end
$$;
