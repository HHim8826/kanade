-- An R2 endpoint as a bucket's settings show it ends with the bucket's name. A bucket store added
-- with it (the dialog said to take the endpoint from there) kept its files in that bucket, below a
-- folder named as its bucket field said, and every listing of it failed: R2 took the bucket's for
-- the reading of a file (NoSuchKey), so checks and scans could not run. The bucket is taken out of
-- the endpoint and the folder says where the files are: each file's path stays what it was, and so
-- do the library's keys of them. A path on other services is theirs (one behind a proxy's path).
WITH s AS (
    SELECT id, rtrim(json_extract(config, '$.endpoint'), '/') AS e FROM stores WHERE kind = 's3'
), r AS (
    SELECT id, substr(e, 1, instr(e, '://') + 2) AS scheme, substr(e, instr(e, '://') + 3) AS rest
    FROM s WHERE instr(e, '://') > 0
), h AS (
    SELECT id, scheme, substr(rest, 1, instr(rest, '/') - 1) AS host, substr(rest, instr(rest, '/') + 1) AS path
    FROM r WHERE instr(rest, '/') > 0
), p AS (
    SELECT id, scheme || host AS endpoint,
        CASE WHEN instr(path, '/') > 0 THEN substr(path, 1, instr(path, '/') - 1) ELSE path END AS bucket,
        CASE WHEN instr(path, '/') > 0 THEN substr(path, instr(path, '/') + 1) ELSE '' END AS under
    FROM h WHERE lower(host) LIKE '%.r2.cloudflarestorage.com' AND path != ''
)
UPDATE stores SET config = json_set(stores.config, '$.endpoint', p.endpoint, '$.bucket', p.bucket, '$.prefix',
    trim(p.under || '/' || coalesce(json_extract(stores.config, '$.bucket'), '') || '/' || coalesce(json_extract(stores.config, '$.prefix'), ''), '/'))
FROM p WHERE stores.id = p.id;
