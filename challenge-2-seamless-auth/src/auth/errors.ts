/**
 * The refresh token was rejected (expired, revoked, reused). Only this error
 * ends a session; network failures and 5xx never do.
 */
export class RefreshRejectedError extends Error {
  constructor(message = 'refresh token rejected') {
    super(message);
    this.name = 'RefreshRejectedError';
  }
}

/** The session is over and the user must sign in again. */
export class SessionExpiredError extends Error {
  constructor() {
    super('session expired');
    this.name = 'SessionExpiredError';
  }
}

/** The user logged out while a request was waiting for a token. */
export class LoggedOutError extends Error {
  constructor() {
    super('logged out');
    this.name = 'LoggedOutError';
  }
}
