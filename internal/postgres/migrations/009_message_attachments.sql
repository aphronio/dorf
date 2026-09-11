alter table dorf.job_messages
    add column attachments jsonb not null default '[]'::jsonb;

alter table dorf.job_messages
    drop constraint job_messages_input_check,
    add constraint job_messages_content_check check (
        length(trim(input)) > 0 or jsonb_array_length(attachments) > 0
    ),
    add constraint job_messages_attachments_array_check check (
        jsonb_typeof(attachments) = 'array'
    );

comment on table dorf.job_messages is
    'Immutable client text and ordered attachment references plus Job-local admission order; follow is FIFO and steer is an explicit priority lane';

insert into dorf.schema_migrations(name) values ('009_message_attachments.sql');
