-- A-143: durable, per-provider outage episodes. Only fixed provider keys and timestamps are stored;
-- wallet errors, RPC responses, addresses and credentials never enter either table.
CREATE TABLE provider_episodes (
 provider text PRIMARY KEY CHECK (provider IN ('pooled_BTC','pooled_XMR','escrow_BTC','escrow_XMR')),
 episode bigint NOT NULL DEFAULT 0 CHECK (episode >= 0),
 bad_since timestamptz,
 alerted boolean NOT NULL DEFAULT false
);
CREATE TABLE provider_episode_deliveries (
 provider text NOT NULL REFERENCES provider_episodes(provider) ON DELETE CASCADE,
 episode bigint NOT NULL,
 user_id text NOT NULL REFERENCES users(id) ON DELETE CASCADE,
 event text NOT NULL CHECK (event IN ('outage','recovery')),
 PRIMARY KEY (provider,episode,user_id,event)
);
