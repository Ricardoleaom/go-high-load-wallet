import { SharedArray } from 'k6/data';

import { WALLETS_CSV, env } from './config.js';

// Seeded by cmd/seed: walletId,playerId with a header line. SharedArray keeps
// one copy for all VUs.
export const wallets = new SharedArray('wallets', () =>
  open(WALLETS_CSV)
    .split('\n')
    .slice(1)
    .filter((line) => line.trim() !== '')
    .map((line) => {
      const [walletId, playerId] = line.trim().split(',');
      return { walletId, playerId };
    }),
);

export function uniformWallet() {
  return wallets[Math.floor(Math.random() * wallets.length)];
}

// Cumulative Zipf weights over wallet ranks, built once and shared: rank 1
// is the hottest wallet. ZIPF_S=1.1 sends a large share of traffic to a
// handful of wallets, which is what row-lock contention looks like.
const zipfCDF = new SharedArray('zipf-cdf', () => {
  const s = parseFloat(env('ZIPF_S', '1.1'));
  const weights = wallets.map((_, rank) => 1 / Math.pow(rank + 1, s));
  const total = weights.reduce((sum, w) => sum + w, 0);
  let cumulative = 0;
  return weights.map((w) => {
    cumulative += w / total;
    return cumulative;
  });
});

export function zipfWallet() {
  const target = Math.random();
  let low = 0;
  let high = zipfCDF.length - 1;
  while (low < high) {
    const mid = (low + high) >> 1;
    if (zipfCDF[mid] < target) {
      low = mid + 1;
    } else {
      high = mid;
    }
  }
  return wallets[low];
}
