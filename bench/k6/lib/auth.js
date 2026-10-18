import http from 'k6/http';
import { check } from 'k6';

import { KEYCLOAK_URL } from './config.js';

function token(client) {
  const res = http.post(`${KEYCLOAK_URL}/realms/wagering/protocol/openid-connect/token`, {
    grant_type: 'client_credentials',
    client_id: client,
    client_secret: `${client}-secret`,
  });
  if (!check(res, { [`token for ${client}`]: (r) => r.status === 200 })) {
    throw new Error(`could not get a token for ${client}: ${res.status} ${res.body}`);
  }
  return res.json('access_token');
}

// The bench clients issue two-hour tokens, longer than any run.
export function fetchTokens() {
  return { provider: token('bench-provider'), internal: token('bench-internal') };
}
