-- Database per service (same server locally, separate clusters in production).
CREATE USER ride_svc     WITH PASSWORD 'ride_svc';
CREATE USER driver_svc   WITH PASSWORD 'driver_svc';
CREATE USER dispatch_svc WITH PASSWORD 'dispatch_svc';
CREATE DATABASE ride_db     OWNER ride_svc;
CREATE DATABASE driver_db   OWNER driver_svc;
CREATE DATABASE dispatch_db OWNER dispatch_svc;
