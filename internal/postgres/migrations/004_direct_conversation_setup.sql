-- Every admitted goal was already retained verbatim as its first Message.
-- Refuse to discard the duplicate if a retained record violates that invariant.
do $$
begin
    if exists (
        select 1 from dorf.jobs j
        where not exists (
            select 1 from dorf.job_messages m
            where m.job_id=j.id and m.from_kind='human' and m.from_id='dorf:initial'
              and m.sequence=1 and m.input=j.goal
        )
    ) then
        raise exception 'Cannot remove Job goal: the original Message is missing or differs';
    end if;
end
$$;

alter table dorf.jobs drop column goal;
alter table dorf.jobs add column agents_md text not null default '';

insert into dorf.schema_migrations(name) values ('004_direct_conversation_setup.sql');
