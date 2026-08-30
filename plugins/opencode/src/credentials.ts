export interface CredentialStore {
  get(): Promise<string | undefined>
  set(credential: string): Promise<void>
  delete(): Promise<void>
}

export class InMemoryCredentialStore implements CredentialStore {
  #credential: string | undefined

  constructor(credential?: string) {
    this.#credential = credential
  }

  async get() {
    return this.#credential
  }

  async set(credential: string) {
    this.#credential = credential
  }

  async delete() {
    this.#credential = undefined
  }
}
