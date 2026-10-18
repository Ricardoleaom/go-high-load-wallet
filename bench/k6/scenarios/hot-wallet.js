// Every request on one wallet: the worst case for row-lock contention.
import { scenarioOptions, env } from '../lib/config.js';
import { fetchTokens } from '../lib/auth.js';
import { wallets } from '../lib/data.js';
import { buildWager, parseMix, pickKind, submit } from '../lib/wager.js';

const SCENARIO = 'hot-wallet';
const mix = parseMix(env('MIX', 'BET:50,WIN:50'));

export const options = scenarioOptions(SCENARIO);
export const setup = fetchTokens;

export default function (tokens) {
  submit(tokens, buildWager(wallets[0], pickKind(mix)), SCENARIO);
}
