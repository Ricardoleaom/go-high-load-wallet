// Mostly reads: 50% wallet balance, 30% ledger page, 20% the baseline
// wager mix.
import { scenarioOptions, env } from '../lib/config.js';
import { fetchTokens } from '../lib/auth.js';
import { uniformWallet } from '../lib/data.js';
import { buildWager, getLedger, getWallet, parseMix, pickKind, submit } from '../lib/wager.js';

const SCENARIO = 'read-mix';
const mix = parseMix(env('MIX', 'BET:60,WIN:25,LOSS:10,REFUND:5'));

export const options = scenarioOptions(SCENARIO);
export const setup = fetchTokens;

export default function (tokens) {
  const wallet = uniformWallet();
  const r = Math.random();
  if (r < 0.5) {
    getWallet(tokens, wallet, SCENARIO);
  } else if (r < 0.8) {
    getLedger(tokens, wallet, SCENARIO);
  } else {
    submit(tokens, buildWager(wallet, pickKind(mix)), SCENARIO);
  }
}
