"""Reproduce Hatchet #4287 and #3922 on the pinned engine (v0.110.8), SDK 1.43.0.

#4287: a task that waits in the queue past schedule_timeout is cancelled
       before it ever runs, and shows up as FAILED with no start time.
#3922: a parent that fans out with aio_run_many, where children hit
       scheduling timeouts, still reports success.

Usage: python repro.py worker   (in one process)
       python repro.py run      (in another)
"""

import asyncio
import sys
import time
from datetime import timedelta

from pydantic import BaseModel

from hatchet_sdk import ConcurrencyExpression, ConcurrencyLimitStrategy, Context, Hatchet

hatchet = Hatchet()


class Item(BaseModel):
    n: int


# One run at a time; each takes 3 s; anything still queued after 5 s times out.
single_lane = ConcurrencyExpression(
    expression="'lane'", max_runs=1, limit_strategy=ConcurrencyLimitStrategy.GROUP_ROUND_ROBIN
)

timeoutee = hatchet.workflow(name="Repro4287", input_validator=Item, concurrency=single_lane)


@timeoutee.task(schedule_timeout=timedelta(seconds=5))
async def slow(input: Item, ctx: Context) -> dict:
    await asyncio.sleep(3)
    return {"n": input.n}


child = hatchet.workflow(name="Repro3922Child", input_validator=Item, concurrency=single_lane)


@child.task(schedule_timeout=timedelta(seconds=5))
async def child_work(input: Item, ctx: Context) -> dict:
    await asyncio.sleep(3)
    return {"n": input.n}


parent = hatchet.workflow(name="Repro3922Parent")


@parent.task(execution_timeout=timedelta(minutes=2))
async def fan_out(input, ctx: Context) -> dict:
    results = await child.aio_run_many([child.create_bulk_run_item(input=Item(n=i)) for i in range(6)])
    return {"children": len(results), "results": results}


async def run() -> None:
    # 4287: 8 runs queued at once into a lane that serves one per 3 s.
    refs = [await timeoutee.aio_run(Item(n=i), wait_for_result=False) for i in range(8)]
    print(f"4287: submitted {len(refs)} runs")
    await asyncio.sleep(45)
    for ref in refs:
        d = await hatchet.runs.aio_get(ref.workflow_run_id)
        t = d.tasks[0] if d.tasks else None
        print(
            f"  run {ref.workflow_run_id[:8]} status={d.run.status} "
            f"started_at={getattr(t, 'started_at', None)} error={getattr(t, 'error_message', None)!r}"
        )

    # 3922: parent fans out 6 children into the same single lane.
    started = time.time()
    try:
        out = await parent.aio_run()
        print(f"3922: parent returned normally after {time.time() - started:.0f}s: {out}")
    except Exception as e:  # noqa: BLE001
        print(f"3922: parent raised after {time.time() - started:.0f}s: {type(e).__name__}: {e}")


if __name__ == "__main__":
    if sys.argv[1] == "worker":
        hatchet.worker("repro-worker", slots=10, workflows=[timeoutee, child, parent]).start()
    else:
        asyncio.run(run())
