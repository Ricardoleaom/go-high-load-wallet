// The baseline mix with Zipf-distributed wallets: a few hot wallets take a
// large share of the traffic, which is where row locks contend.
import { scenarioOptions, env } from '../lib/config.js';
import { fetchTokens } from '../lib/auth.js';
import { zipfWallet } from '../lib/data.js';
import { buildWager, parseMix, pickKind, submit } from '../lib/wager.js';

const SCENARIO = 'zipf-mix';
const mix = parseMix(env('MIX', 'BET:60,WIN:25,LOSS:10,REFUND:5'));

export const options = scenarioOptions(SCENARIO);
export const setup = fetchTokens;

export default function (tokens) {
  const wallet = zipfWallet();
  submit(tokens, buildWager(wallet, pickKind(mix)), SCENARIO);
}
