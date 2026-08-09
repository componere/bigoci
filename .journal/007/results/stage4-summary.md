## ghcr — cold-push — 1GiB file

Median MB/s.

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256MiB | 89.4 | 93.8 | 78.0 |
| 512MiB | 57.3 | 98.6 | 92.0 |

## ghcr — cold-pull — 1GiB file

Median MB/s.

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256MiB | 88.6 | 145.0 | 155.2 |
| 512MiB | 58.5 | 86.9 | 88.7 |

## All populations

| registry | scenario | part | workers | file | parts | n | median | mean | stddev | min | max | fail | 429/503 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| ghcr | cold-push | 256MiB | 2 | 1GiB | 4 | 2 | 89.4 | 89.4 | 2.4 | 87.0 | 91.9 | 0 | 0 |
| ghcr | cold-pull | 256MiB | 2 | 1GiB | 4 | 2 | 88.6 | 88.6 | 4.1 | 84.5 | 92.7 | 0 | 0 |
| ghcr | cold-push | 256MiB | 4 | 1GiB | 4 | 2 | 93.8 | 93.8 | 6.6 | 87.1 | 100.4 | 0 | 0 |
| ghcr | cold-pull | 256MiB | 4 | 1GiB | 4 | 2 | 145.0 | 145.0 | 13.7 | 131.3 | 158.6 | 0 | 0 |
| ghcr | cold-push | 256MiB | 8 | 1GiB | 4 | 2 | 78.0 | 78.0 | 3.5 | 74.5 | 81.4 | 0 | 0 |
| ghcr | cold-pull | 256MiB | 8 | 1GiB | 4 | 2 | 155.2 | 155.2 | 2.7 | 152.4 | 157.9 | 0 | 0 |
| ghcr | cold-push | 512MiB | 2 | 1GiB | 2 | 2 | 57.3 | 57.3 | 42.4 | 15.0 | 99.7 | 0 | 0 |
| ghcr | cold-pull | 512MiB | 2 | 1GiB | 2 | 2 | 58.5 | 58.5 | 32.9 | 25.6 | 91.4 | 0 | 0 |
| ghcr | cold-push | 512MiB | 4 | 1GiB | 2 | 2 | 98.6 | 98.6 | 2.8 | 95.8 | 101.4 | 0 | 0 |
| ghcr | cold-pull | 512MiB | 4 | 1GiB | 2 | 2 | 86.9 | 86.9 | 2.0 | 84.9 | 88.9 | 0 | 0 |
| ghcr | cold-push | 512MiB | 8 | 1GiB | 2 | 2 | 92.0 | 92.0 | 5.1 | 86.9 | 97.1 | 0 | 0 |
| ghcr | cold-pull | 512MiB | 8 | 1GiB | 2 | 2 | 88.7 | 88.7 | 2.8 | 85.9 | 91.5 | 0 | 0 |

