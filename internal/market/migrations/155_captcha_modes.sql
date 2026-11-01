-- A-725 (owner decision 2026-10-31). captcha_required gains a third value, 'under_attack', the new default: no image on
-- sign-in unless the global attack signal is on, and a written question on registration. 'true' keeps its meaning, the
-- image on both forms, and 'false' stays off. Migration 010 seeded 'true', so 'true' alone cannot tell an operator's
-- choice from the seed; a 'Turned on CAPTCHA' audit row can, so such a market stays on 'true'. Every other 'true' moves
-- to the new default. An operator who kept the seed on purpose, or whose audit row has aged out of retention, chooses
-- again on /admin/security (UPGRADING.md). An earlier binary reads 'under_attack' as the image required.
UPDATE settings SET value='under_attack'
 WHERE key='captcha_required' AND value='true'
   AND NOT EXISTS (SELECT 1 FROM audit_events WHERE action LIKE 'Turned on CAPTCHA%');
