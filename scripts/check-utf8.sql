-- Lists every stored text value that is not valid UTF-8, and fails (psql exits 3) when there is one (UPGRADING.md,
-- section 42; QA5W3B-1). Read-only. Run it against the database to check, with the market stopped:
--   psql "$DATABASE_URL" -X -f scripts/check-utf8.sql
-- It works in any server encoding. A SQL_ASCII or LATIN1 database accepts any bytes; the market before A-481 wrote
-- UTF-8 into it but never checked form input, so a value can hold bytes that are not UTF-8 (a review "Great \xff
-- seller"). pg_restore copies such bytes into a new UTF8 database without complaint, and that database's own next
-- backup then fails to restore ("COPY failed for table ...: invalid byte sequence for encoding "UTF8""). This check
-- reads, in every table outside the system schemas, each column of type text, varchar or char, of an array of one of
-- those (element by element) or of a domain over any of these (QA5W5B-2), as the bytes stored (convert_to(...,
-- 'SQL_ASCII') converts nothing), and asks PostgreSQL to verify them as UTF-8. These are the only types the market
-- stores text in.
-- Each bad value is named by table and column (quoted as an identifier where SQL needs it, an array element with its
-- subscript: key_images[2]) and by its row: the primary key as a condition to paste after WHERE (QA5W4B-4: a row's ctid
-- changes when it is updated, so a row with two bad values needs its key), and the ctid for a table without one. Then
-- how to correct it (QA5W4B-3, QA5W5B-3): with no valid multibyte UTF-8 in it, reading its bytes as the database's own
-- single-byte encoding (LATIN1 for a SQL_ASCII database, which records none) is a safe correction; with some, that
-- reading would double-encode the valid part ("Crème" would become "CrÃ¨me"), and in a database whose encoding is
-- itself multibyte no reading is safe: type the text it should hold. Last, its first 40 bytes escaped (\377 is the byte
-- 0xff). Correct each with an UPDATE while the market is stopped, then run this again.
\set ON_ERROR_STOP on
DO $check$
DECLARE
  col record;
  r record;
  keys text;
  key_condition text;
  source text;
  shown_key text;
  shown_value text;
  hint text;
  raw bytea;
  lead int;
  multibyte boolean;
  server_encoding text := current_setting('server_encoding');
  reading text;
  columns int := 0;
  bad int := 0;
BEGIN
  -- The encoding a bad value's bytes are safely read as: the database's own when it is single-byte (WIN1252 text read as
  -- LATIN1 would turn its quotes into control characters), LATIN1 for SQL_ASCII; none when it is multibyte.
  IF server_encoding = 'SQL_ASCII' THEN
    reading := 'LATIN1';
  ELSIF pg_encoding_max_length(pg_char_to_encoding(server_encoding)) = 1 THEN
    reading := server_encoding;
  END IF;
  FOR col IN
    -- Each column's type with its domains resolved (a domain over a domain names the inner one as its base type) and,
    -- for an array, its element type's: in_array then.
    WITH RECURSIVE walk(rel, attnum, typ, in_array) AS (
      SELECT a.attrelid, a.attnum, a.atttypid, false
      FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid JOIN pg_namespace n ON n.oid = c.relnamespace
      WHERE c.relkind = 'r' AND n.nspname <> 'information_schema' AND n.nspname NOT LIKE 'pg\_%'
        AND a.attnum > 0 AND NOT a.attisdropped
      UNION ALL
      SELECT w.rel, w.attnum, CASE WHEN t.typtype = 'd' THEN t.typbasetype ELSE t.typelem END, w.in_array OR t.typtype <> 'd'
      FROM walk w JOIN pg_type t ON t.oid = w.typ
      WHERE t.typtype = 'd' OR (NOT w.in_array AND t.typcategory = 'A' AND t.typelem <> 0)
    )
    SELECT c.oid AS rel, n.nspname, c.relname, a.attname, w.in_array
    FROM walk w JOIN pg_attribute a ON a.attrelid = w.rel AND a.attnum = w.attnum
      JOIN pg_class c ON c.oid = w.rel JOIN pg_namespace n ON n.oid = c.relnamespace
    WHERE w.typ IN ('text'::regtype, 'varchar'::regtype, 'bpchar'::regtype)
    ORDER BY n.nspname, c.relname, a.attnum
  LOOP
    columns := columns + 1;
    -- The column's values, each with where it sits in the column: an array is read element by element, each named by
    -- its subscript.
    IF col.in_array THEN
      source := format('FROM %I.%I t CROSS JOIN LATERAL (SELECT e.value::text AS value, CASE WHEN array_ndims(t.%3$I) = 1 THEN format(''[%%s]'', array_lower(t.%3$I, 1) + e.n - 1) ELSE format('' (element %%s of a multidimensional array, in storage order)'', e.n) END AS position FROM unnest(t.%3$I) WITH ORDINALITY AS e(value, n)) v',
        col.nspname, col.relname, col.attname);
    ELSE
      source := format('FROM %I.%I t CROSS JOIN LATERAL (SELECT t.%I::text AS value, '''' AS position) v', col.nspname, col.relname, col.attname);
    END IF;
    BEGIN
      -- The whole column at once; only a column that fails is read row by row.
      EXECUTE format('SELECT count(convert(convert_to(v.value, ''SQL_ASCII''), ''UTF8'', ''UTF8'')) %s', source);
      CONTINUE;
    EXCEPTION WHEN character_not_in_repertoire THEN
      NULL;
    END;
    -- The primary key's columns, and an expression that writes the row's key as a condition: id = 'abc' AND n = '2'.
    SELECT string_agg(format('%I', a.attname), ', ' ORDER BY k.ord),
        string_agg(format('format(%L, t.%I::text)', format('%I = %%L', a.attname), a.attname), ' || '' AND '' || ' ORDER BY k.ord)
      INTO keys, key_condition
    FROM pg_index i CROSS JOIN LATERAL unnest(i.indkey) WITH ORDINALITY AS k(attnum, ord)
    JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
    WHERE i.indrelid = col.rel AND i.indisprimary;
    FOR r IN EXECUTE format('SELECT t.ctid::text AS loc, %s AS key, v.value AS val, v.position AS pos %s WHERE v.value IS NOT NULL',
        coalesce(key_condition, 'NULL::text'), source)
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
          multibyte := false;
          FOR i IN 0 .. length(raw) - 1 LOOP
            lead := get_byte(raw, i);
            CONTINUE WHEN lead < 194 OR lead > 244;
            BEGIN
              PERFORM convert(substring(raw FROM i + 1 FOR CASE WHEN lead < 224 THEN 2 WHEN lead < 240 THEN 3 ELSE 4 END), 'UTF8', 'UTF8');
              multibyte := true;
              EXIT;
            EXCEPTION WHEN character_not_in_repertoire THEN
              NULL;
            END;
          END LOOP;
          IF reading IS NULL THEN
            hint := format('this database''s encoding, %s, is multibyte, so no reading of its bytes is a safe correction: type the text it should hold', server_encoding);
          ELSIF multibyte THEN
            hint := format('it also holds valid multibyte UTF-8, which reading it as %s would double-encode: type the text it should hold', reading);
          ELSIF server_encoding = 'SQL_ASCII' THEN
            hint := 'no valid multibyte UTF-8 in it, so reading its bytes as LATIN1 is a safe correction, or as the single-byte encoding they were written in if that was another (a SQL_ASCII database does not record it; WIN1252, for one)';
          ELSE
            hint := format('no valid multibyte UTF-8 in it, so reading its bytes as %s, this database''s encoding, is a safe correction', reading);
          END IF;
        EXCEPTION WHEN character_not_in_repertoire THEN
          shown_value := '(not shown)';
          hint := 'its bytes cannot be read in a UTF8 database: correct it in the old database';
        END;
        RAISE NOTICE 'not valid UTF-8: table %, column %, row % (ctid %); %; starting: %',
          format('%I.%I', col.nspname, col.relname), format('%I', col.attname) || r.pos, shown_key, r.loc, hint, shown_value;
      END;
    END LOOP;
  END LOOP;
  IF bad > 0 THEN
    RAISE EXCEPTION '% stored text value(s) are not valid UTF-8 (listed above). Correct each while the market is stopped and run this check again; see UPGRADING.md, section 42.', bad;
  END IF;
  RAISE NOTICE 'Every value in the % text column(s) is valid UTF-8.', columns;
END
$check$;
