# A2A pilot report

Calibration: `CAPACITY=2.1333 BUSY=r1@0.05=0.02 r2@0.1=0.07 r3@0.2=0.06 r4@0.4=0.18 r5@0.8=0.38 FIX_RATE=1.000 PER_ISSUE=add-sign=23/23 clamp=23/23 count-vowels=22/22 fizzbuzz=22/22 is-even=23/23 max-of=23/23 mean=22/22 reverse-words=23/23
CI_RUNNERS=103 CI_RATE_PER_MIN=431 CI_BURST=72`

Latency in seconds, end to end, completed workflows only. `small_p99`: the two small tenants together.
`burst_p99`: workflows submitted during the burst. `gpu_busy`: mean running sequences / capacity.

## Every run

| run | submitted | completed | failed | unfinished | p50 | p99 | small_p99 | burst_p99 | req_per_wf | ci_req_per_wf | backoff_s | repeated_exec | ci_refused | gpu_busy | gpu_waiting | cpu_busy |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| burst-A-1 | 828 | 828 | 0 | 0 | 163 | 337 | 245 | 374 | 17.7 | 12.7 | 14956 | 0 | 5592 | 0.68 | 117.8 | 0.01 |
| burst-A-2 | 828 | 828 | 0 | 0 | 162 | 318 | 245 | 341 | 17.1 | 12.1 | 12864 | 0 | 5045 | 0.66 | 114.9 | 0.01 |
| burst-B-1 | 828 | 828 | 0 | 0 | 122 | 310 | 148 | 339 | 10.0 | 5.0 | 0 | 0 | 415 | 0.67 | 0.0 | 0.02 |
| burst-B-2 | 828 | 828 | 0 | 0 | 121 | 309 | 149 | 343 | 10.0 | 5.0 | 0 | 0 | 372 | 0.67 | 0.0 | 0.02 |
| burst-C-1 | 828 | 828 | 0 | 0 | 121 | 311 | 149 | 338 | 10.0 | 5.0 | 0 | 0 | 84 | 0.69 | 0.0 | 0.02 |
| burst-C-2 | 828 | 828 | 0 | 0 | 123 | 309 | 149 | 341 | 10.0 | 5.0 | 0 | 0 | 85 | 0.68 | 0.0 | 0.02 |
| flood-A-1 | 1530 | 1001 | 529 | 0 | 212 | 333 | 309 | – | 12.4 | 7.4 | 3073 | 0 | 1792 | 0.86 | 304.6 | 0.01 |
| flood-A-2 | 1530 | 1010 | 520 | 0 | 211 | 329 | 308 | – | 12.4 | 7.4 | 3236 | 0 | 1765 | 0.85 | 301.9 | 0.02 |
| flood-B-1 | 1530 | 959 | 0 | 571 | 183 | 459 | 104 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.96 | 0.0 | 0.03 |
| flood-B-2 | 1530 | 970 | 0 | 560 | 186 | 457 | 105 | – | 10.7 | 5.7 | 0 | 0 | 10 | 0.95 | 0.0 | 0.03 |
| flood-C-1 | 1530 | 952 | 0 | 578 | 184 | 465 | 105 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.93 | 0.0 | 0.02 |
| flood-C-2 | 1530 | 954 | 0 | 576 | 181 | 464 | 106 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.93 | 0.0 | 0.02 |
| normal-A-1 | 714 | 714 | 0 | 0 | 53 | 104 | 103 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.59 | 0.0 | 0.01 |
| normal-A-2 | 714 | 714 | 0 | 0 | 53 | 104 | 103 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.59 | 0.0 | 0.01 |
| normal-B-1 | 714 | 714 | 0 | 0 | 53 | 104 | 103 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.60 | 0.0 | 0.02 |
| normal-B-2 | 714 | 714 | 0 | 0 | 53 | 104 | 103 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.58 | 0.0 | 0.02 |
| normal-C-1 | 714 | 714 | 0 | 0 | 53 | 104 | 103 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.59 | 0.0 | 0.02 |
| normal-C-2 | 714 | 714 | 0 | 0 | 53 | 104 | 103 | – | 10.7 | 5.7 | 0 | 0 | 0 | 0.60 | 0.0 | 0.01 |

## normal

Each cell: the runs' values, then their mean.

| | A | B | C |
|---|---|---|---|
| completed | 714 / 714 (mean 714) | 714 / 714 (mean 714) | 714 / 714 (mean 714) |
| unfinished | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| p50 | 53 / 53 (mean 53) | 53 / 53 (mean 53) | 53 / 53 (mean 53) |
| p99 | 104 / 104 (mean 104) | 104 / 104 (mean 104) | 104 / 104 (mean 104) |
| big_p50 | 53 / 53 (mean 53) | 53 / 53 (mean 53) | 53 / 53 (mean 53) |
| small_p50 | 44 / 44 (mean 44) | 44 / 44 (mean 44) | 44 / 44 (mean 44) |
| small_p99 | 103 / 103 (mean 103) | 103 / 103 (mean 103) | 103 / 103 (mean 103) |
| burst_p99 | – / – | – / – | – / – |
| req_per_wf | 10.7 / 10.7 (mean 10.7) | 10.7 / 10.7 (mean 10.7) | 10.7 / 10.7 (mean 10.7) |
| llm_req_per_wf | 2.0 / 2.0 (mean 2.0) | 2.0 / 2.0 (mean 2.0) | 2.0 / 2.0 (mean 2.0) |
| ci_req_per_wf | 5.7 / 5.7 (mean 5.7) | 5.7 / 5.7 (mean 5.7) | 5.7 / 5.7 (mean 5.7) |
| backoff_s | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| repeated_exec | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| ci_dup_starts | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| ci_refused | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| sbx_refused | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| gpu_busy | 0.59 / 0.59 (mean 0.59) | 0.60 / 0.58 (mean 0.59) | 0.59 / 0.60 (mean 0.59) |
| cpu_busy | 0.01 / 0.01 (mean 0.01) | 0.02 / 0.02 (mean 0.02) | 0.02 / 0.01 (mean 0.01) |

## burst

Each cell: the runs' values, then their mean.

| | A | B | C |
|---|---|---|---|
| completed | 828 / 828 (mean 828) | 828 / 828 (mean 828) | 828 / 828 (mean 828) |
| unfinished | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| p50 | 163 / 162 (mean 163) | 122 / 121 (mean 121) | 121 / 123 (mean 122) |
| p99 | 337 / 318 (mean 328) | 310 / 309 (mean 310) | 311 / 309 (mean 310) |
| big_p50 | 169 / 167 (mean 168) | 216 / 213 (mean 215) | 217 / 218 (mean 218) |
| small_p50 | 152 / 150 (mean 151) | 74 / 74 (mean 74) | 73 / 74 (mean 73) |
| small_p99 | 245 / 245 (mean 245) | 148 / 149 (mean 148) | 149 / 149 (mean 149) |
| burst_p99 | 374 / 341 (mean 357) | 339 / 343 (mean 341) | 338 / 341 (mean 340) |
| req_per_wf | 17.7 / 17.1 (mean 17.4) | 10.0 / 10.0 (mean 10.0) | 10.0 / 10.0 (mean 10.0) |
| llm_req_per_wf | 2.0 / 2.0 (mean 2.0) | 2.0 / 2.0 (mean 2.0) | 2.0 / 2.0 (mean 2.0) |
| ci_req_per_wf | 12.7 / 12.1 (mean 12.4) | 5.0 / 5.0 (mean 5.0) | 5.0 / 5.0 (mean 5.0) |
| backoff_s | 14956 / 12864 (mean 13910) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| repeated_exec | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| ci_dup_starts | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| ci_refused | 5592 / 5045 (mean 5318) | 415 / 372 (mean 394) | 84 / 85 (mean 84) |
| sbx_refused | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| gpu_busy | 0.68 / 0.66 (mean 0.67) | 0.67 / 0.67 (mean 0.67) | 0.69 / 0.68 (mean 0.69) |
| cpu_busy | 0.01 / 0.01 (mean 0.01) | 0.02 / 0.02 (mean 0.02) | 0.02 / 0.02 (mean 0.02) |

## flood

Each cell: the runs' values, then their mean.

| | A | B | C |
|---|---|---|---|
| completed | 1001 / 1010 (mean 1006) | 959 / 970 (mean 964) | 952 / 954 (mean 953) |
| unfinished | 0 / 0 (mean 0) | 571 / 560 (mean 566) | 578 / 576 (mean 577) |
| p50 | 212 / 211 (mean 211) | 183 / 186 (mean 184) | 184 / 181 (mean 182) |
| p99 | 333 / 329 (mean 331) | 459 / 457 (mean 458) | 465 / 464 (mean 465) |
| big_p50 | 212 / 211 (mean 212) | 300 / 300 (mean 300) | 303 / 300 (mean 302) |
| small_p50 | 211 / 210 (mean 211) | 48 / 48 (mean 48) | 49 / 49 (mean 49) |
| small_p99 | 309 / 308 (mean 308) | 104 / 105 (mean 104) | 105 / 106 (mean 106) |
| burst_p99 | – / – | – / – | – / – |
| req_per_wf | 12.4 / 12.4 (mean 12.4) | 10.7 / 10.7 (mean 10.7) | 10.7 / 10.7 (mean 10.7) |
| llm_req_per_wf | 2.0 / 2.0 (mean 2.0) | 2.0 / 2.0 (mean 2.0) | 2.0 / 2.0 (mean 2.0) |
| ci_req_per_wf | 7.4 / 7.4 (mean 7.4) | 5.7 / 5.7 (mean 5.7) | 5.7 / 5.7 (mean 5.7) |
| backoff_s | 3073 / 3236 (mean 3155) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| repeated_exec | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| ci_dup_starts | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| ci_refused | 1792 / 1765 (mean 1778) | 0 / 10 (mean 5) | 0 / 0 (mean 0) |
| sbx_refused | 0 / 0 (mean 0) | 0 / 0 (mean 0) | 0 / 0 (mean 0) |
| gpu_busy | 0.86 / 0.85 (mean 0.85) | 0.96 / 0.95 (mean 0.95) | 0.93 / 0.93 (mean 0.93) |
| cpu_busy | 0.01 / 0.02 (mean 0.01) | 0.03 / 0.03 (mean 0.03) | 0.02 / 0.02 (mean 0.02) |

