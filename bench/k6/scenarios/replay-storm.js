// The baseline mix where REPLAY_RATIO of the requests resend an earlier
// request unchanged, as a provider retrying after a timeout would.
import { scenarioOptions, env } from '../lib/config.js';
import { fetchTokens } from '../lib/auth.js';
import { uniformWallet } from '../lib/data.js';
import { buildWager, parseMix, pickKind, submit } from '../lib/wager.js';

const SCENARIO = 'replay-storm';
const mix = parseMix(env('MIX', 'BET:60,WIN:25,LOSS:10,REFUND:5'));
const replayRatio = parseFloat(env('REPLAY_RATIO', '0.1'));
const sent = [];

export const options = scenarioOptions(SCENARIO);
export const setup = fetchTokens;

export default function (tokens) {
  if (sent.length > 0 && Math.random() < replayRatio) {
    submit(tokens, sent[Math.floor(Math.random() * sent.length)], SCENARIO);
    return;
  }
  const body = buildWager(uniformWallet(), pickKind(mix));
  sent.push(body);
  if (sent.length > 200) {
    sent.shift();
  }
  submit(tokens, body, SCENARIO);
}
