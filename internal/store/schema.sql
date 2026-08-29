-- Расширение всегда в public: тип vector должен быть виден из любой схемы,
-- в которой лежат таблицы кэша.
create extension if not exists vector with schema public;

-- Записи, векторы и теги живут в одной базе намеренно: инвалидация обязана
-- убирать вектор той же операцией, что payload, иначе ANN-поиск продолжает
-- возвращать мёртвые записи и они занимают места в top-k.
create table if not exists entries (
    id          text primary key,
    prompt_hash text not null,
    prompt      text not null,
    payload     text not null,
    -- lang заполняется при записи по полному тексту ответа, где текста хватает
    -- для надёжного определения. На пятисловном запросе определитель языка
    -- ненадёжен — см. пост M2 про остаточную утечку language_switch.
    lang        text not null default '',
    embedding   vector(:dims) not null,
    created_at  timestamptz not null default now(),
    expires_at  timestamptz
);

create index if not exists entries_prompt_hash_idx on entries (prompt_hash);
create index if not exists entries_embedding_idx on entries
    using hnsw (embedding vector_cosine_ops);

-- Индекса по expires_at здесь быть не должно, и это не экономия места.
-- С ним у планировщика появляется обходной путь для ANN-запроса: взять по
-- нему все живые строки bitmap-сканом и отсортировать точно, минуя HNSW.
-- Ответы остаются верными, а задержка вырастает на два порядка — на корпусе
-- в 20 тысяч записей это 1 мс против 110 мс.
drop index if exists entries_expires_at_idx;

-- on delete cascade даёт атомарность даром: одна DELETE по entries убирает
-- payload, вектор и теги в одной транзакции.
create table if not exists entry_tags (
    entry_id text not null references entries (id) on delete cascade,
    tag      text not null,
    primary key (entry_id, tag)
);

create index if not exists entry_tags_tag_idx on entry_tags (tag);
