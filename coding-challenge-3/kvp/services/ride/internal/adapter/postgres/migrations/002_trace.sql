-- W3C traceparent of the request that created the ride: every later step of
-- the ride continues this trace, so one trace id shows the whole journey.
ALTER TABLE rides ADD COLUMN trace_parent text;
