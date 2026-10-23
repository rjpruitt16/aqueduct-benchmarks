# Reproducing #4287 and #3922

Runs on a laptop with Docker and [uv](https://docs.astral.sh/uv/).

```sh
docker compose up -d
# wait for http://localhost:8888/api/ready to return 200
docker compose exec -T hatchet-lite /hatchet-admin token create \
  --config /config --tenant-id 707d0855-80ab-4e1f-a156-f1c4546cbf52 | tail -1 > token.txt

uv venv --python 3.12 .venv
uv pip install --python .venv/bin/python 'hatchet-sdk==1.43.0'

export HATCHET_CLIENT_TOKEN=$(cat token.txt) HATCHET_CLIENT_TLS_STRATEGY=none HATCHET_CLIENT_HOST_PORT=localhost:7077
.venv/bin/python repro.py worker &
.venv/bin/python repro.py run

docker compose down -v
```

Both workflows share a lane that runs one 3-second task at a time, and each task has a 5-second scheduling timeout.

## Result on v0.110.8 (2026-10-23)

```
4287: submitted 8 runs
  run ae00cda9 status=COMPLETED started_at=2026-10-23 03:00:21 error=''
  run 22f3e1e1 status=COMPLETED started_at=2026-10-23 03:00:24 error=''
  run a4ce45d0 status=FAILED started_at=None error=''
  ... 5 more FAILED, started_at=None, error=''
3922: parent returned normally after 6s:
  {'fan_out': {'children': 6, 'results': [{'child_work': {'n': 0}}, {'child_work': {'n': 1}}, {}, {}, {}, {}]}}
```

The timed-out runs carry a `SCHEDULING_TIMED_OUT` task event in Hatchet's `v1_task_events_olap` table, but nothing in the REST run details says why they failed.
