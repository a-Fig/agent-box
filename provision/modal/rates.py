"""Dated Modal pricing constants for runner-v1 cost estimation.

Sourced from https://modal.com/pricing on 2026-11-27.
Keep this module stdlib-only (no modal import) so unit tests run anywhere.
"""

RATES_UPDATED = "2026-11-27"
RATES_SOURCE = "modal.com/pricing"

# Standard 1x Modal Function compute rates (USD per second).
# Modal bills CPU in physical cores (1 physical core = 2 vCPUs).
CPU_CORE_SEC_USD = 0.0000131
MEM_GIB_SEC_USD = 0.00000222

# Non-preemptible Modal Sandbox compute multiplier (3x CPU and memory).
# Published Sandbox rates on 2026-11-27 are ~$0.00003942/core-s and ~$0.00000667/GiB-s.
SANDBOX_CPU_MEM_MULTIPLIER = 3.0

# GPU per-second rates (USD per second, 1x in both Function and Sandbox modes).
GPU_RATES_USD_PER_SEC: dict[str, float] = {
    "T4": 0.000164,
    "L4": 0.000222,
    "A10G": 0.000306,
    "L40S": 0.000542,
    "A100-40GB": 0.000583,
    "A100-80GB": 0.000694,
    "H100": 0.001097,
}

DEFAULT_GPU_TYPE = "T4"
