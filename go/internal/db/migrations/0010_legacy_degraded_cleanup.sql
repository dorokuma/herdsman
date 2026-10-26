UPDATE agent_events SET status='invalidated', deliverable=0, invalidated_reason='legacy_degraded' WHERE json_extract(payload_json,'$.degraded')=1 AND status != 'invalidated';
