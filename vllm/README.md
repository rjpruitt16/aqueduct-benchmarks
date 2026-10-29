# vLLM: Aquifer in front of a GPU

Everything else in this repository puts Aquifer in front of a CPU service. This target puts it in front of a real GPU serving a model, and asks the question behind [the GPU retry tax](https://rahmipruitt.me/content/gpu-retry-tax/): when callers time out and retry, how much of what the GPU generates is thrown away, and does holding the work in a queue change that?

Both experiments ran in August 2026 against one RunPod RTX PRO 4500 Blackwell running vLLM with `Qwen/Qwen2.5-1.5B-Instruct`:

- `--max-num-seqs 48`;
- the KV cache deliberately capped at 512 MB, so a realistic burst can fill it;
- every request generates exactly 300 tokens (`min_tokens = max_tokens = 300`), so each one holds the GPU long enough to matter.

They first appeared in Aquifer's `benchmark.md` (sections 9 and 10). The harness in [`harness/`](harness/) is copied unchanged from [rjpruitt16/aquifer](https://github.com/rjpruitt16/aquifer) `benchmark/` at `25c53b5`.

## 1. A fixed burst: 40 requests/s for 30 s

Aquifer v0.5.0. Aquifer paced dispatch from the ORCA load signal vLLM already sends on its responses (`kv_cache_usage_perc`), with no Aquifer-specific headers configured.

| | Straight to vLLM | Through Aquifer |
|---|---|---|
| Requests that succeeded | 100% | 100% |
| Caller's wait, mean / p99 | 11.2 s / 20.1 s | 1.0 ms / 3.2 ms to be accepted |
| Most requests waiting inside vLLM | 449 | 8 |
| Highest KV cache use | 69.4% | 59.1% |

- **vLLM doesn't refuse, it queues.** Straight to vLLM, every request succeeded, but vLLM's own queue grew to 449 and callers waited up to 20 s. That is the shape of slowness that makes a real caller time out and retry, adding load to a GPU that is already behind.
- **Through Aquifer, vLLM's queue stayed at 8 or under.** Aquifer took each request in about a millisecond and sent it on at a pace vLLM could keep up with.
- **The Aquifer column measures acceptance, not results.** "1.0 ms" is how long Aquifer took to accept a job, not how long the caller waited for the generated text. This run didn't measure time to result through Aquifer, so the two latency rows don't compare like for like.
- **The ORCA signal does steer.** With Aquifer's own ceiling raised above what vLLM can sustain (80 requests/s and 120 in flight, against vLLM's 48) and 60 requests/s offered for 40 s, KV cache use cycled 22% → 79% → 22%. Each time the cache crossed 70%, ORCA told Aquifer to drop to 2 requests/s; the backlog drained, the cache fell, and Aquifer ramped back up.

## 2. Waste, measured in tokens: a ramp from 20 to 300 requests/s

Aquifer v0.8.0 (`400a9f2`). The first experiment's callers waited patiently, so no retry ever made the GPU do the same work twice. This one uses callers that behave like real ones. Straight to vLLM, a caller gives up after 5 s and retries, up to 3 times. Through Aquifer, a caller submits once and waits up to 60 s on the job's stream. Offered load ramps from 20 to 300 requests/s over 60 s, far past what one GPU can serve.

Three numbers from vLLM's own `/metrics` and the client's log:

- **Raw tokens:** everything the GPU generated (`generation_tokens_total`), including work for callers that had already given up.
- **Useful tokens:** tokens that reached a caller who was still waiting.
- **Wasted:** raw minus useful.

| | Straight to vLLM, retrying callers | Through Aquifer, at most 45 in flight |
|---|---|---|
| Requests offered | 9,537 | 9,585 |
| Answered while the caller waited | 0.9% | 25.2% |
| **Generation wasted** | **96.5%** | **22.0%** |
| Most requests waiting inside vLLM | 18,079 | 0 |

- **Straight to vLLM, almost everything the GPU did was thrown away.** The GPU was busy the whole time, so its throughput looked fine. But 96.5% of its tokens went to attempts whose caller had already timed out and sent the request again, so the GPU kept generating duplicates of work nobody would read.
- **Through Aquifer, waste fell to 22% and vLLM's queue stayed empty.** Callers didn't resend work that was already running, because each one submitted once and waited. Aquifer held the backlog and sent work at most 45 at a time, just under vLLM's 48.
- **Most requests still went unanswered, because the load was far beyond one GPU.** 300 requests/s of 300-token generations is many times this GPU's capacity. Through Aquifer, a quarter of the requests finished within their caller's 60 s; the rest hadn't finished by then.
- **Aquifer's 22% isn't all lost.** It counts tokens whose caller had stopped waiting. Aquifer keeps those results, so a caller that comes back with the same key, or a webhook, still gets them. For callers like that, the real waste is lower.

## Limits

- **One run each, and the raw data wasn't kept.** Only the numbers above survive. Rerunning needs a GPU; the commands are below.
- **The direct callers are impatient.** A 5 s timeout with 3 retries is a harsh client. A more patient one would waste less straight to vLLM; that wasn't tested.
- **Aquifer's limit was set by hand.** Its 45-in-flight ceiling came from knowing vLLM's `--max-num-seqs`. ORCA's signal tracks KV memory, not scheduler slots, so a GPU that runs out of slots before memory can be saturated while the signal stays quiet. Detecting that limit automatically is [aquifer#11](https://github.com/rjpruitt16/aquifer/issues/11).
- **Older Aquifer.** These are v0.5.0 and v0.8.0, before the shared destination limits and ready estimates used in the [Envoy](../envoy/RESULTS.md) runs.

## Running

Needs a vLLM instance (RunPod or anywhere) and an Aquifer that can reach it.

```sh
cd vllm/harness
# 1. A fixed burst (vegeta); arguments: baseline rate, burst rate, max tokens.
./gpu_retry_tax.sh <vllm-url> <aquifer-url> 40 40 300

# 2. Waste in tokens: a ramp with retrying callers, then the report.
pip install -r requirements.txt
python3 retry_tax_harness.py --mode direct --vllm-url <vllm-url> \
  --start-rate 20 --end-rate 300 --ramp-seconds 60 --client-timeout 5 --max-retries 3 --max-tokens 300
python3 retry_tax_harness.py --mode aquifer --vllm-url <vllm-url> --aquifer-url <aquifer-url> \
  --start-rate 20 --end-rate 300 --ramp-seconds 60 --max-tokens 300
python3 analyze_retry_tax.py --events <events.jsonl> --metrics <metrics.jsonl>
```
