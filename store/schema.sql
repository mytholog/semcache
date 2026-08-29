-- Расширение всегда в public: тип vector должен быть виден из любой схемы,
-- в которой лежат таблицы кэша.
create extension if not exists vector with schema public;

-- Записи, векторы и теги живут в одной базе намеренно: инвалидация обязана
-- убирать вектор той же операцией, что payload, иначе ANN-поиск продолжает
-- возвращать мёртвые записи и они занимают места в top-k.
create table if not exists entries (
    id          text primary key,
    -- namespace изолирует то, что нельзя путать: модель, версия шаблона,
    -- арендатор. Фильтр по нему в ANN-ветке ведёт себя так же, как любой
    -- другой фильтр поверх HNSW (см. пост M3): при нескольких сопоставимых
    -- по размеру пространствах имён это несколько пунктов recall, при
    -- сильном перекосе стоит включать iterative scan.
    namespace   text not null default '',
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

-- Колонки, появившиеся после первой версии схемы: create table if not exists
-- на живой базе не добавит ни одной, и запрос сломается на отсутствующем поле.
alter table entries add column if not exists namespace text not null default '';
alter table entries add column if not exists lang text not null default '';

-- Индекс только по prompt_hash, и namespace в него не входит намеренно —
-- это тот же капкан, что с expires_at, в третий раз. Любой btree, которым
-- можно удовлетворить предикат ANN-ветки, даёт планировщику обходной путь:
-- он берёт по нему все подходящие строки и сортирует их точно, мимо HNSW.
-- Ответы остаются верными, задержка растёт на два порядка — 114 мс против
-- 1 мс. Порядок колонок не спасает: (prompt_hash, namespace) планировщик
-- обходит полным сканом индекса.
--
-- Точному поиску namespace в индексе и не нужен: prompt_hash — хеш промпта,
-- он уже отбирает единицы строк, и namespace проверяется по куче.
create index if not exists entries_prompt_hash_idx on entries (prompt_hash);
drop index if exists entries_lookup_idx;
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
