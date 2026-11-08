DROP TABLE web_sessions;
DELETE FROM auth_flows WHERE client_type='web';
ALTER TABLE auth_flows DROP CONSTRAINT web_flow_not_exchangeable;
ALTER TABLE auth_flows DROP COLUMN return_path;
ALTER TABLE auth_flows DROP COLUMN client_type;
