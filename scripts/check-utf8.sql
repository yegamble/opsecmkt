-- Lists every stored text value that is not valid UTF-8, and fails (psql exits 3) when there is one (UPGRADING.md,
-- section 42; QA5W3B-1). Read-only. Run it against the database to check, with the market stopped:
--   psql "$DATABASE_URL" -X -f scripts/check-utf8.sql
-- It works in any server encoding. A SQL_ASCII or LATIN1 database accepts any bytes; the market before A-481 wrote
-- UTF-8 into it but never checked form input, so a value can hold bytes that are not UTF-8 (a review "Great \xff
-- seller"). pg_restore copies such bytes into a new UTF8 database without complaint, and that database's own next
-- backup then fails to restore ("COPY failed for table ...: invalid byte sequence for encoding "UTF8""). This check
-- reads each text, varchar and char column of every table outside the system schemas as the bytes stored
-- (convert_to(..., 'SQL_ASCII') converts nothing) and asks PostgreSQL to verify them as UTF-8.
-- Each bad value is named by table, column and its row: the primary key as a condition to paste after WHERE (QA5W4B-4:
-- a row's ctid changes when it is updated, so a row with two bad values needs its key), and the ctid for a table
-- without one. Then whether it also holds valid multibyte UTF-8 (QA5W4B-3): if not, reading its bytes as LATIN1 is a
-- safe correction; if so, that reading would double-encode the valid part ("Crème" would become "CrÃ¨me"), so type
-- the text it should hold. Last, its first 40 bytes escaped (\377 is the byte 0xff). Correct each with an UPDATE while
-- the market is stopped, then run this again.
\set ON_ERROR_STOP on
DO $check$
DECLARE
  col record;
  r record;
  keys text;
  key_condition text;
  shown_key text;
  shown_value text;
  hint text;
  raw bytea;
  lead int;
  columns int := 0;
  bad int := 0;
BEGIN
  FOR col IN
    SELECT c.oid AS rel, n.nspname, c.relname, a.attname
    FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE c.relkind = 'r' AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
      AND a.attnum > 0 AND NOT a.attisdropped
      AND a.atttypid IN ('text'::regtype, 'varchar'::regtype, 'bpchar'::regtype)
    ORDER BY n.nspname, c.relname, a.attnum
  LOOP
    columns := columns + 1;
    BEGIN
      -- The whole column at once; only a column that fails is read row by row.
      EXECUTE format('SELECT count(convert(convert_to(%I, ''SQL_ASCII''), ''UTF8'', ''UTF8'')) FROM %I.%I', col.attname, col.nspname, col.relname);
      CONTINUE;
    EXCEPTION WHEN character_not_in_repertoire THEN
      NULL;
    END;
    -- The primary key's columns, and an expression that writes the row's key as a condition: id = 'abc' AND n = '2'.
    SELECT string_agg(format('%I', a.attname), ', ' ORDER BY k.ord),
        string_agg(format('format(%L, %I::text)', format('%I = %%L', a.attname), a.attname), ' || '' AND '' || ' ORDER BY k.ord)
      INTO keys, key_condition
    FROM pg_index i CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
    JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
    WHERE i.indrelid = col.rel AND i.indisprimary;
    FOR r IN EXECUTE format('SELECT ctid::text AS loc, %s AS key, %I AS val FROM %I.%I WHERE %I IS NOT NULL',
        coalesce(key_condition, 'NULL::text'), col.attname, col.nspname, col.relname, col.attname)
    LOOP
      BEGIN
        PERFORM convert(convert_to(r.val, 'SQL_ASCII'), 'UTF8', 'UTF8');
      EXCEPTION WHEN character_not_in_repertoire THEN
        bad := bad + 1;
        -- In a UTF8 database bytes that are not UTF-8 cannot be read, even escaped. A key that is not plain ASCII is
        -- not shown either: escaped, it would no longer be the condition to paste.
        BEGIN
          IF r.key IS NULL THEN
            shown_key := '(no primary key: use the ctid)';
          ELSIF encode(convert_to(r.key, 'SQL_ASCII'), 'escape') = r.key THEN
            shown_key := 'where ' || r.key;
          ELSE
            shown_key := keys || ' (not shown: use the ctid)';
          END IF;
        EXCEPTION WHEN character_not_in_repertoire THEN
          shown_key := keys || ' (not shown: use the ctid)';
        END;
        BEGIN
          raw := convert_to(r.val, 'SQL_ASCII');
          shown_value := encode(substring(raw FROM 1 FOR 40), 'escape');
          -- Valid multibyte UTF-8 anywhere in it: a lead byte (0xc2 to 0xf4) starting a sequence PostgreSQL accepts.
          hint := 'no valid multibyte UTF-8 in it, so reading its bytes as LATIN1 is a safe correction';
          FOR i IN 0 .. length(raw) - 1 LOOP
            lead := get_byte(raw, i);
            CONTINUE WHEN lead < 194 OR lead > 244;
            BEGIN
              PERFORM convert(substring(raw FROM i + 1 FOR CASE WHEN lead < 224 THEN 2 WHEN lead < 240 THEN 3 ELSE 4 END), 'UTF8', 'UTF8');
              hint := 'it also holds valid multibyte UTF-8, which reading it as LATIN1 would double-encode: type the text it should hold';
              EXIT;
            EXCEPTION WHEN character_not_in_repertoire THEN
              NULL;
            END;
          END LOOP;
        EXCEPTION WHEN character_not_in_repertoire THEN
          shown_value := '(not shown)';
          hint := 'its bytes cannot be read in a UTF8 database: correct it in the old database';
        END;
        RAISE NOTICE 'not valid UTF-8: table %.%, column %, row % (ctid %); %; starting: %',
          col.nspname, col.relname, col.attname, shown_key, r.loc, hint, shown_value;
      END;
    END LOOP;
  END LOOP;
  IF bad > 0 THEN
    RAISE EXCEPTION '% stored text value(s) are not valid UTF-8 (listed above). Correct each while the market is stopped and run this check again; see UPGRADING.md, section 42.', bad;
  END IF;
  RAISE NOTICE 'Every value in the % text column(s) is valid UTF-8.', columns;
END
$check$;
