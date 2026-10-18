import http from 'k6/http';
import { check } from 'k6';

import { BASE_URL, env } from './config.js';

// Business rejections (422) and pending reversals (202) are valid outcomes,
// not failures: only transport errors and 5xx count toward http_req_failed.
http.setResponseCallback(http.expectedStatuses(200, 201, 202, 422));

const PROVIDER = 'bench-provider';
const AMOUNT_MIN = Math.round(parseFloat(env('AMOUNT_MIN', '1.00')) * 100);
const AMOUNT_MAX = Math.round(parseFloat(env('AMOUNT_MAX', '50.00')) * 100);

// MIX=BET:60,WIN:25,LOSS:10,REFUND:5
export function parseMix(raw) {
  const entries = raw.split(',').map((part) => {
    const [kind, weight] = part.split(':');
    return { kind: kind.trim(), weight: parseFloat(weight) };
  });
  const total = entries.reduce((sum, e) => sum + e.weight, 0);
  let cumulative = 0;
  return entries.map((e) => {
    cumulative += e.weight / total;
    return { kind: e.kind, until: cumulative };
  });
}

export function pickKind(mix) {
  const r = Math.random();
  return (mix.find((m) => r < m.until) || mix[mix.length - 1]).kind;
}

function randomAmount(kind) {
  if (kind === 'LOSS') {
    return '0.00';
  }
  const cents = AMOUNT_MIN + Math.floor(Math.random() * (AMOUNT_MAX - AMOUNT_MIN + 1));
  return `${Math.floor(cents / 100)}.${String(cents % 100).padStart(2, '0')}`;
}

// Bets this VU placed and has not refunded yet: a REFUND must point at a
// BET of the same wallet, round and amount to be applicable.
const recentBets = [];
const RECENT_LIMIT = 50;

export function buildWager(wallet, kind) {
  if (kind === 'REFUND') {
    const bet = recentBets.splice(Math.floor(Math.random() * recentBets.length), 1)[0];
    if (!bet) {
      return buildWager(wallet, 'BET');
    }
    return { ...bet, externalTransactionId: crypto.randomUUID(), kind: 'REFUND', referenceExternalTransactionId: bet.externalTransactionId };
  }

  const body = {
    providerId: PROVIDER,
    externalTransactionId: crypto.randomUUID(),
    playerId: wallet.playerId,
    walletId: wallet.walletId,
    roundId: `round-${crypto.randomUUID()}`,
    gameId: 'bench-game',
    kind,
    money: { amount: randomAmount(kind), currency: 'BRL' },
  };
  if (kind === 'BET') {
    recentBets.push(body);
    if (recentBets.length > RECENT_LIMIT) {
      recentBets.shift();
    }
  }
  return body;
}

export function submit(tokens, body, scenario) {
  const res = http.post(`${BASE_URL}/wagering/transactions`, JSON.stringify(body), {
    headers: {
      Authorization: `Bearer ${tokens.provider}`,
      'Content-Type': 'application/json',
      'Idempotency-Key': `${PROVIDER}:${body.externalTransactionId}`,
    },
    tags: { kind: body.kind, scenario, name: 'POST /wagering/transactions' },
  });
  check(res, { 'wager accepted or rejected by business rules': (r) => [200, 202, 422].includes(r.status) });
  return res;
}

export function getWallet(tokens, wallet, scenario) {
  const res = http.get(`${BASE_URL}/wallets/${wallet.walletId}`, {
    headers: { Authorization: `Bearer ${tokens.internal}` },
    tags: { scenario, name: 'GET /wallets/{id}' },
  });
  check(res, { 'wallet read': (r) => r.status === 200 });
  return res;
}

export function getLedger(tokens, wallet, scenario) {
  const res = http.get(`${BASE_URL}/wallets/${wallet.walletId}/ledger?limit=20`, {
    headers: { Authorization: `Bearer ${tokens.internal}` },
    tags: { scenario, name: 'GET /wallets/{id}/ledger' },
  });
  check(res, { 'ledger read': (r) => r.status === 200 });
  return res;
}
