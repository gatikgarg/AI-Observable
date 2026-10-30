# SQL conventions

Dashboards query `calls_v`, never `calls`. `calls_v.ts` is Unix seconds (UTC),
not milliseconds. Source timestamps in `calls.ts_ms` are UTC epoch milliseconds.

The addendum dashboards use `ts BETWEEN $__from/1000 AND $__to/1000` for every
query. Their adaptive bucket expression is `MAX(10,(($__to-$__from)/1000)/120)`;
this avoids relying on plugin-specific `$__interval` interpolation and targets
roughly 120 points. Model labels use `provider || '/' || model`.
