// Wagers spread uniformly over every seeded wallet: the baseline mix.
import { scenarioOptions, env } from '../lib/config.js';
import { fetchTokens } from '../lib/auth.js';
import { uniformWallet } from '../lib/data.js';
import { buildWager, parseMix, pickKind, submit } from '../lib/wager.js';

const SCENARIO = 'uniform-mix';
const mix = parseMix(env('MIX', 'BET:60,WIN:25,LOSS:10,REFUND:5'));

export const options = scenarioOptions(SCENARIO);
export const setup = fetchTokens;

export default function (tokens) {
  const wallet = uniformWallet();
  submit(tokens, buildWager(wallet, pickKind(mix)), SCENARIO);
}
