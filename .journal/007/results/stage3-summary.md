## dist — cold-push — 8GiB file

Median MB/s.

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256MiB | 678.4 | 758.8 | 917.1 |
| 512MiB | 655.8 | 907.1 | 908.4 |

## dist — cold-pull — 8GiB file

Median MB/s.

| part \ workers | 2 | 4 | 8 |
|---|---|---|---|
| 256MiB | 722.6 | 717.1 | 724.7 |
| 512MiB | 727.0 | 719.5 | 719.2 |

## All populations

| registry | scenario | part | workers | file | parts | n | median | mean | stddev | min | max | fail | 429/503 |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| dist | cold-push | 256MiB | 2 | 8GiB | 32 | 3 | 678.4 | 664.1 | 20.5 | 635.1 | 678.8 | 0 | 0 |
| dist | cold-pull | 256MiB | 2 | 8GiB | 32 | 3 | 722.6 | 723.8 | 2.6 | 721.5 | 727.5 | 0 | 0 |
| dist | cold-push | 256MiB | 4 | 8GiB | 32 | 3 | 758.8 | 759.5 | 17.9 | 737.9 | 781.8 | 0 | 0 |
| dist | cold-pull | 256MiB | 4 | 8GiB | 32 | 3 | 717.1 | 718.9 | 2.9 | 716.5 | 723.1 | 0 | 0 |
| dist | cold-push | 256MiB | 8 | 8GiB | 32 | 3 | 917.1 | 912.0 | 11.0 | 896.7 | 922.1 | 0 | 0 |
| dist | cold-pull | 256MiB | 8 | 8GiB | 32 | 3 | 724.7 | 723.2 | 2.4 | 719.8 | 725.2 | 0 | 0 |
| dist | cold-push | 512MiB | 2 | 8GiB | 16 | 3 | 655.8 | 654.9 | 1.8 | 652.5 | 656.5 | 0 | 0 |
| dist | cold-pull | 512MiB | 2 | 8GiB | 16 | 3 | 727.0 | 728.3 | 2.2 | 726.5 | 731.4 | 0 | 0 |
| dist | cold-push | 512MiB | 4 | 8GiB | 16 | 3 | 907.1 | 905.9 | 2.5 | 902.4 | 908.3 | 0 | 0 |
| dist | cold-pull | 512MiB | 4 | 8GiB | 16 | 3 | 719.5 | 718.8 | 3.4 | 714.4 | 722.6 | 0 | 0 |
| dist | cold-push | 512MiB | 8 | 8GiB | 16 | 3 | 908.4 | 904.9 | 5.0 | 897.9 | 908.5 | 0 | 0 |
| dist | cold-pull | 512MiB | 8 | 8GiB | 16 | 3 | 719.2 | 719.0 | 2.0 | 716.5 | 721.4 | 0 | 0 |

