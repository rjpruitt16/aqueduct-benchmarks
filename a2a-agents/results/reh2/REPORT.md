# A2A pilot report

Calibration: `CAPACITY=1.2727 BUSY=r2@0.15=0.15 r3@0.5=0.39 FIX_RATE=1.000 PER_ISSUE=add-sign=5/5 clamp=5/5 count-vowels=5/5 fizzbuzz=5/5 is-even=5/5 max-of=5/5 mean=4/4 reverse-words=5/5
CI_RUNNERS=62 CI_RATE_PER_MIN=257 CI_BURST=43`

Latency in seconds, end to end, completed workflows only. `small_p99`: the two small tenants together.
`burst_p99`: workflows submitted during the burst. `gpu_busy`: mean running sequences / capacity.

## Every run

| run | submitted | completed | failed | unfinished | p50 | p99 | small_p99 | burst_p99 | req_per_wf | ci_req_per_wf | backoff_s | repeated_exec | ci_refused | gpu_busy | gpu_waiting | cpu_busy |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| burst-A-1 | 123 | 123 | 0 | 0 | 78 | 153 | 105 | 156 | 12.4 | 5.9 | 0 | 0 | 0 | 0.56 | 14.3 | 0.01 |
| burst-B-1 | 123 | 123 | 0 | 0 | 74 | 167 | 81 | 169 | 12.4 | 5.9 | 0 | 0 | 0 | 0.53 | 0.0 | 0.01 |
| burst-C-1 | 123 | 123 | 0 | 0 | 74 | 167 | 81 | 168 | 12.4 | 5.9 | 0 | 0 | 0 | 0.53 | 0.0 | 0.01 |
| flood-A-1 | 228 | 227 | 0 | 1 | 121 | 197 | 191 | – | 14.6 | 8.1 | 1274 | 0 | 624 | 0.69 | 48.8 | 0.01 |
| flood-B-1 | 228 | 228 | 0 | 0 | 133 | 200 | 109 | – | 11.5 | 5.0 | 0 | 0 | 186 | 0.69 | 0.0 | 0.02 |
| flood-C-1 | 228 | 228 | 0 | 0 | 130 | 198 | 108 | – | 11.5 | 5.0 | 0 | 0 | 28 | 0.69 | 0.0 | 0.02 |
| normal-A-1 | 106 | 106 | 0 | 0 | 56 | 109 | 69 | – | 12.4 | 5.9 | 0 | 0 | 0 | 0.52 | 0.0 | 0.01 |
| normal-B-1 | 106 | 106 | 0 | 0 | 56 | 109 | 69 | – | 12.4 | 5.9 | 0 | 0 | 0 | 0.51 | 0.0 | 0.01 |
| normal-C-1 | 106 | 106 | 0 | 0 | 56 | 109 | 69 | – | 12.4 | 5.9 | 0 | 0 | 0 | 0.52 | 0.0 | 0.01 |

## normal

Each cell: the runs' values, then their mean.

| | A | B | C |
|---|---|---|---|
| completed | 106 | 106 | 106 |
| unfinished | 0 | 0 | 0 |
| p50 | 56 | 56 | 56 |
| p99 | 109 | 109 | 109 |
| big_p50 | 59 | 59 | 59 |
| small_p50 | 49 | 50 | 49 |
| small_p99 | 69 | 69 | 69 |
| burst_p99 | – | – | – |
| req_per_wf | 12.4 | 12.4 | 12.4 |
| llm_req_per_wf | 2.4 | 2.4 | 2.4 |
| ci_req_per_wf | 5.9 | 5.9 | 5.9 |
| backoff_s | 0 | 0 | 0 |
| repeated_exec | 0 | 0 | 0 |
| ci_dup_starts | 0 | 0 | 0 |
| ci_refused | 0 | 0 | 0 |
| sbx_refused | 0 | 0 | 0 |
| gpu_busy | 0.52 | 0.51 | 0.52 |
| cpu_busy | 0.01 | 0.01 | 0.01 |

## burst

Each cell: the runs' values, then their mean.

| | A | B | C |
|---|---|---|---|
| completed | 123 | 123 | 123 |
| unfinished | 0 | 0 | 0 |
| p50 | 78 | 74 | 74 |
| p99 | 153 | 167 | 167 |
| big_p50 | 78 | 88 | 88 |
| small_p50 | 79 | 60 | 61 |
| small_p99 | 105 | 81 | 81 |
| burst_p99 | 156 | 169 | 168 |
| req_per_wf | 12.4 | 12.4 | 12.4 |
| llm_req_per_wf | 2.4 | 2.4 | 2.4 |
| ci_req_per_wf | 5.9 | 5.9 | 5.9 |
| backoff_s | 0 | 0 | 0 |
| repeated_exec | 0 | 0 | 0 |
| ci_dup_starts | 0 | 0 | 0 |
| ci_refused | 0 | 0 | 0 |
| sbx_refused | 0 | 0 | 0 |
| gpu_busy | 0.56 | 0.53 | 0.53 |
| cpu_busy | 0.01 | 0.01 | 0.01 |

## flood

Each cell: the runs' values, then their mean.

| | A | B | C |
|---|---|---|---|
| completed | 227 | 228 | 228 |
| unfinished | 1 | 0 | 0 |
| p50 | 121 | 133 | 130 |
| p99 | 197 | 200 | 198 |
| big_p50 | 125 | 145 | 144 |
| small_p50 | 120 | 51 | 51 |
| small_p99 | 191 | 109 | 108 |
| burst_p99 | – | – | – |
| req_per_wf | 14.6 | 11.5 | 11.5 |
| llm_req_per_wf | 2.4 | 2.4 | 2.4 |
| ci_req_per_wf | 8.1 | 5.0 | 5.0 |
| backoff_s | 1274 | 0 | 0 |
| repeated_exec | 0 | 0 | 0 |
| ci_dup_starts | 0 | 0 | 0 |
| ci_refused | 624 | 186 | 28 |
| sbx_refused | 0 | 0 | 0 |
| gpu_busy | 0.69 | 0.69 | 0.69 |
| cpu_busy | 0.01 | 0.02 | 0.02 |

