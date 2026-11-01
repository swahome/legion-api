# Running legion-api on Windows

These steps build and run the API on Windows 10 or 11 with PowerShell. Nothing
here needs Docker, WSL or `make`: the API is one `.exe` plus a `.env` file.

## 1. Install Go, PostgreSQL and Git

Open PowerShell and run:

```powershell
winget install --id GoLang.Go
winget install --id PostgreSQL.PostgreSQL.16
winget install --id Git.Git
```

The PostgreSQL installer asks for a password for the `postgres` superuser.
Write it down; you need it in step 2. Keep the default port, 5432.

**Close PowerShell and open a new window** so the new `PATH` is picked up, then
check:

```powershell
go version        # must be at least the version in go.mod (1.26)
git --version
```

The PostgreSQL tools are not put on `PATH` by the installer. Either call them by
their full path, as below, or add the folder for this session:

```powershell
$env:Path += ";C:\Program Files\PostgreSQL\16\bin"
```

## 2. Create the database

```powershell
psql -U postgres -h localhost
```

Enter the `postgres` password, then at the `postgres=#` prompt:

```sql
CREATE ROLE legion LOGIN PASSWORD 'legion';
CREATE DATABASE legion OWNER legion;
\q
```

Use a stronger password than `legion` anywhere other than your own machine. If
the password contains `@`, `:`, `/`, `#` or `%`, percent-encode it in
`DATABASE_URL` (for example `@` becomes `%40`).

## 3. Get the code and make a `.env`

```powershell
cd D:\code            # anywhere you like
git clone https://github.com/swahome/legion-api.git
cd legion-api
Copy-Item .env.example .env
notepad .env
```

Generate a JWT secret (works in Windows PowerShell 5.1 and PowerShell 7):

```powershell
$b = New-Object byte[] 48
[Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($b)
[Convert]::ToBase64String($b)
```

Set at least these lines in `.env`:

```ini
LEGION_ADDR=127.0.0.1:8089
DATABASE_URL=postgres://legion:legion@localhost:5432/legion?sslmode=disable
LEGION_JWT_SECRET=<paste the secret from above>

# false while you use plain http://localhost; true behind HTTPS
LEGION_COOKIE_SECURE=false

LEGION_TIMEZONE=Africa/Nairobi
LEGION_WEB_URL=http://localhost:5173

# Africa's Talking. Leave all three commented out in development and texts
# (with their codes) are written to the API log instead. To test real
# sending, use the sandbox account:
# LEGION_AT_USERNAME=sandbox
# LEGION_AT_API_KEY=<your sandbox API key>
# LEGION_AT_SENDER_ID=
```

`LEGION_AT_USERNAME` and `LEGION_AT_API_KEY` must be set together or not at all;
the API refuses to start with only one. `.env.example` lists every other setting.

## 4. Build the `.exe`

Don't use `make build` here: the Makefile builds a Linux binary by default. Build
directly with Go:

```powershell
$env:CGO_ENABLED = "0"
go build -trimpath -ldflags "-s -w -X main.version=dev" -o bin\legion-api.exe .\cmd\legion-api
```

The result, `bin\legion-api.exe`, has the migrations and the time zone database
built in. Copy it with `.env` to any Windows machine; Go is not needed there.

## 5. Migrate, create an admin, run

The binary reads `.env` from the **current folder**, so run these from the folder
that holds `.env` (the repo root here):

```powershell
.\bin\legion-api.exe migrate
.\bin\legion-api.exe create-user -phone 0700000001 -password secret123 -admin
.\bin\legion-api.exe
```

To keep `.env` somewhere else, point at it first:
`$env:LEGION_ENV_FILE = "C:\legion\.env"`.

Check that it is up, from a second PowerShell window:

```powershell
curl.exe http://127.0.0.1:8089/healthz
```

Stop it with `Ctrl+C`; it shuts down cleanly.

To load the legacy data: `.\bin\legion-api.exe import-legacy -dump C:\path\to\anonymized2.sql`
(see `docs/data/legacy-import.md`).

## Running the web app against it

In legion-web, the Vite dev server proxies `/v1` to `LEGION_API_URL`, which
defaults to `http://localhost:8080`. With the API on 8089, set it in legion-web's
`.env` file:

```ini
LEGION_API_URL=http://127.0.0.1:8089
```

Then `npm run dev` there and open `http://localhost:5173`. That address must
match `LEGION_WEB_URL` in the API's `.env`.

## Running the tests

`make test` uses `-race`, which on Windows needs a C compiler. Without one, run:

```powershell
$env:TEST_DATABASE_URL = "postgres://legion:legion@localhost:5432/legion_test?sslmode=disable"
go test -count=1 ./...
```

Create `legion_test` the same way as `legion` in step 2. The tests don't read
`.env`, so the variable has to be set in the shell. Without it, the database
tests are skipped.

## Windows gotchas

- **`curl` is not curl** in Windows PowerShell 5.1; it is an alias for
  `Invoke-WebRequest`. Type `curl.exe`.
- **"go is not recognized"** or **"psql is not recognized"**: open a new
  PowerShell window after installing, or add `C:\Program Files\PostgreSQL\16\bin`
  to `PATH` as in step 1.
- **Firewall prompt**: with `LEGION_ADDR=127.0.0.1:8089` the API only listens on
  loopback and Windows doesn't ask. If you set `:8089` it listens on every
  network card and the firewall prompt appears; keep loopback unless another
  machine really needs to reach it.
- **Port already in use**: find what holds it with
  `Get-NetTCPConnection -LocalPort 8089 | Select-Object OwningProcess`, then
  `Get-Process -Id <that number>`.
- **`password authentication failed for user "legion"`**: the password in
  `DATABASE_URL` doesn't match the role, or it has a special character that needs
  percent-encoding.
- **Script execution is disabled**: none of these steps run `.ps1` scripts, but
  if you add one, run `Set-ExecutionPolicy -Scope CurrentUser RemoteSigned` once.
- **Run it in the background or at boot**: the binary isn't a Windows service on
  its own. Wrap it with [NSSM](https://nssm.cc) (`nssm install legion-api`, set
  the path to the `.exe` and the startup folder to the folder with `.env`), or
  use Task Scheduler with "Start in" set to that folder.
