## ghcr — cold-push — 4GiB file

Median aggregate MB/s. Columns are configured workers; capped cells show their maximum active workers.

| part \ workers | 4 | 8 |
|---|---|---|
| 256MiB | 111.2 | 112.3 |
| 512MiB | 109.0 | 109.5 |

## ghcr — cold-pull — 4GiB file

Median aggregate MB/s. Columns are configured workers; capped cells show their maximum active workers.

| part \ workers | 4 | 8 |
|---|---|---|
| 256MiB | 163.2 | 273.4 |
| 512MiB | 161.6 | 262.1 |

## All populations

| registry | scenario | part | configured | max active | file | parts | n | median | mean | stddev | min | max | fail | 429/503 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| ghcr | cold-push | 256MiB | 4 | 4 | 4GiB | 16 | 3 | 111.2 | 112.2 | 2.4 | 109.8 | 115.5 | 0 | 0 |
| ghcr | cold-pull | 256MiB | 4 | 4 | 4GiB | 16 | 3 | 163.2 | 159.2 | 6.2 | 150.5 | 164.0 | 0 | 0 |
| ghcr | cold-push | 256MiB | 8 | 8 | 4GiB | 16 | 3 | 112.3 | 115.2 | 4.2 | 112.2 | 121.2 | 0 | 0 |
| ghcr | cold-pull | 256MiB | 8 | 8 | 4GiB | 16 | 3 | 273.4 | 262.2 | 20.8 | 233.0 | 280.2 | 0 | 0 |
| ghcr | cold-push | 512MiB | 4 | 4 | 4GiB | 8 | 3 | 109.0 | 109.6 | 1.2 | 108.6 | 111.2 | 0 | 0 |
| ghcr | cold-pull | 512MiB | 4 | 4 | 4GiB | 8 | 3 | 161.6 | 156.6 | 10.6 | 141.8 | 166.3 | 0 | 0 |
| ghcr | cold-push | 512MiB | 8 | 8 | 4GiB | 8 | 3 | 109.5 | 106.9 | 4.0 | 101.2 | 109.9 | 0 | 0 |
| ghcr | cold-pull | 512MiB | 8 | 8 | 4GiB | 8 | 3 | 262.1 | 255.6 | 17.6 | 231.5 | 273.0 | 0 | 0 |

