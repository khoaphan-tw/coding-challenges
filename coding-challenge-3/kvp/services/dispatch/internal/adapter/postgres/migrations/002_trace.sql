-- The ride's trace (copied from RideRequested): accept, reject, timeouts and
-- retries continue it, so one trace id shows the whole dispatch journey.
ALTER TABLE dispatch_sagas ADD COLUMN trace_parent text;
