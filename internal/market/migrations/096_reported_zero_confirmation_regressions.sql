-- QA10W-2: before migration 056, credited deposits already flagged at depth zero
-- were reported just like those at negative depth. Carry those existing episodes
-- forward without sending a duplicate regression notice after upgrade. A flagged
-- uncredited deposit is a different review case and must not be changed.
UPDATE payments SET regress_notice = true
WHERE credited AND flagged AND confirmations < 1 AND NOT regress_notice;
