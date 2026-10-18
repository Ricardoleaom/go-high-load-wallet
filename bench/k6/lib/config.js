// Settings shared by every scenario, read from environment variables so the
// orchestrator (bench/run.sh) can drive one script at many rates.

export function env(key, fallback) {
  const value = __ENV[key];
  return value === undefined || value === '' ? fallback : value;
}

export function intEnv(key, fallback) {
  const value = parseInt(env(key, String(fallback)), 10);
  if (Number.isNaN(value)) {
    throw new Error(`${key} must be an integer`);
  }
  return value;
}

export const BASE_URL = env('BASE_URL', 'http://app:8080');
export const KEYCLOAK_URL = env('KEYCLOAK_URL', 'http://keycloak:8080');
export const WALLETS_CSV = env('WALLETS_CSV', '/data/wallets.csv');

// Open-model executors: requests arrive at the configured rate whether or
// not earlier ones finished, so a slow server shows up as latency and
// dropped iterations instead of silently lowering the load (coordinated
// omission). VUs are capped for the 8 GB reference machine; running out of
// them is itself recorded as dropped iterations.
function executor() {
  const mode = env('MODE', 'steady');
  const vus = (rate) => ({
    preAllocatedVUs: Math.min(Math.max(Math.ceil(rate / 10), 10), 200),
    maxVUs: Math.min(Math.max(Math.ceil(rate / 2), 20), 300),
  });

  if (mode === 'knee') {
    const start = intEnv('KNEE_START', 50);
    const step = intEnv('KNEE_STEP', 50);
    const max = intEnv('KNEE_MAX', 600);
    const hold = env('KNEE_STEP_DURATION', '60s');
    const stages = [];
    for (let rate = start; rate <= max; rate += step) {
      stages.push({ target: rate, duration: '5s' }, { target: rate, duration: hold });
    }
    return { executor: 'ramping-arrival-rate', startRate: start, timeUnit: '1s', stages, ...vus(max) };
  }
  if (mode !== 'steady') {
    throw new Error(`MODE must be steady or knee, got ${mode}`);
  }

  const rate = intEnv('RATE', 50);
  return { executor: 'constant-arrival-rate', rate, timeUnit: '1s', duration: env('DURATION', '60s'), ...vus(rate) };
}

export function scenarioOptions(name) {
  return {
    scenarios: { [name]: executor() },
    // Recorded, never aborting: the report decides what a failure means.
    thresholds: { http_req_failed: ['rate<0.001'] },
    summaryTrendStats: ['avg', 'min', 'med', 'p(90)', 'p(95)', 'p(99)', 'max'],
    setupTimeout: '60s',
  };
}
