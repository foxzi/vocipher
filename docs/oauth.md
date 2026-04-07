# OAuth2 / OpenID Connect / OAuth2 / OpenID Connect

## English

Vocala supports external authentication via OAuth2 and OpenID Connect (OIDC). Users can sign in with Google, GitHub, Keycloak, Authentik, GitLab, or any OIDC-compatible provider.

### How It Works

1. User clicks "Sign in with Google" (or other provider) on the login page
2. Browser redirects to the provider's authorization page
3. User authorizes the application
4. Provider redirects back to Vocala with an authorization code
5. Vocala exchanges the code for a token and fetches user info (email, name)
6. If the user exists (by OAuth ID or email), they are logged in
7. If the user is new, an account is auto-created

### General Configuration

In `config.yaml`:

```yaml
oauth:
  enabled: true
  providers:
    - name: ProviderName        # Display name on login button
      client_id: "xxx"          # From provider's developer console
      client_secret: "xxx"      # From provider's developer console
      auth_url: "https://..."   # Authorization endpoint
      token_url: "https://..."  # Token endpoint
      userinfo_url: "https://..." # UserInfo endpoint
      scopes: ["openid", "email", "profile"]
      auto_activate: true       # Auto-activate new OAuth users (default: false)
```

Multiple providers can be configured simultaneously. Each will appear as a separate button on the login page.

**Callback URL** for all providers: `https://YOUR_DOMAIN/auth/oauth/PROVIDER_NAME/callback`

For example, if your domain is `voice.example.com` and provider name is `Google`:
```
https://voice.example.com/auth/oauth/Google/callback
```

### Important Notes

- Provider `name` in config must match the callback URL (case-sensitive)
- `auto_activate: true` -- new OAuth users are immediately active (recommended for trusted providers like corporate Google Workspace)
- `auto_activate: false` -- new OAuth users need admin activation before they can use Vocala
- If an OAuth user's email matches an existing local account, the accounts are linked automatically
- OAuth users don't have a password -- they can only log in via their OAuth provider
- The first user (whether local or OAuth) automatically becomes admin

---

## Google

### Step 1: Create OAuth Client

1. Go to [Google Cloud Console](https://console.cloud.google.com/)
2. Create a new project or select an existing one
3. Navigate to **APIs & Services** > **Credentials**
4. Click **Create Credentials** > **OAuth client ID**
5. If prompted, configure the **OAuth consent screen**:
   - User Type: **External** (or Internal for Google Workspace)
   - App name: `Vocala`
   - Authorized domains: `your-domain.com`
6. Create OAuth client:
   - Application type: **Web application**
   - Name: `Vocala`
   - Authorized redirect URIs: `https://YOUR_DOMAIN/auth/oauth/Google/callback`
7. Copy **Client ID** and **Client Secret**

### Step 2: Configure Vocala

```yaml
oauth:
  enabled: true
  providers:
    - name: Google
      client_id: "123456789-abcdef.apps.googleusercontent.com"
      client_secret: "GOCSPX-xxxxxxxxxxxxx"
      auth_url: "https://accounts.google.com/o/oauth2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      userinfo_url: "https://www.googleapis.com/oauth2/v3/userinfo"
      scopes: ["openid", "email", "profile"]
      auto_activate: true
```

### Step 3: Restrict to Your Organization (Optional)

To allow only users from your Google Workspace domain, add the `hd` parameter:

```yaml
      auth_url: "https://accounts.google.com/o/oauth2/auth?hd=yourcompany.com"
```

---

## GitHub

### Step 1: Create OAuth App

1. Go to [GitHub Developer Settings](https://github.com/settings/developers)
2. Click **OAuth Apps** > **New OAuth App**
3. Fill in:
   - Application name: `Vocala`
   - Homepage URL: `https://YOUR_DOMAIN`
   - Authorization callback URL: `https://YOUR_DOMAIN/auth/oauth/GitHub/callback`
4. Click **Register application**
5. Copy **Client ID**
6. Click **Generate a new client secret** and copy it

### Step 2: Configure Vocala

```yaml
oauth:
  enabled: true
  providers:
    - name: GitHub
      client_id: "Iv1.xxxxxxxxxxxx"
      client_secret: "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"
      auth_url: "https://github.com/login/oauth/authorize"
      token_url: "https://github.com/login/oauth/access_token"
      userinfo_url: "https://api.github.com/user"
      scopes: ["user:email"]
      auto_activate: true
```

### Step 3: Restrict to Your Organization (Optional)

GitHub OAuth does not have built-in organization restriction in the OAuth flow. However, you can:
- Set `auto_activate: false` and manually activate approved users
- Use a GitHub App instead of OAuth App for more granular control

---

## Authentik

[Authentik](https://goauthentik.io/) is a self-hosted identity provider. It supports OIDC natively.

### Step 1: Create Provider in Authentik

1. Log in to Authentik admin panel
2. Go to **Applications** > **Providers** > **Create**
3. Select **OAuth2/OpenID Provider**
4. Fill in:
   - Name: `Vocala`
   - Authorization flow: **default-provider-authorization-explicit-consent**
   - Client type: **Confidential**
   - Redirect URIs: `https://YOUR_DOMAIN/auth/oauth/Authentik/callback`
   - Scopes: select **openid**, **email**, **profile**
5. Note down **Client ID** and **Client Secret**

### Step 2: Create Application in Authentik

1. Go to **Applications** > **Applications** > **Create**
2. Fill in:
   - Name: `Vocala`
   - Slug: `vocala`
   - Provider: select the provider created above

### Step 3: Configure Vocala

Replace `authentik.your-domain.com` with your Authentik URL:

```yaml
oauth:
  enabled: true
  providers:
    - name: Authentik
      client_id: "xxxxxxxxxxxxxxxxxxxx"
      client_secret: "xxxxxxxxxxxxxxxxxxxx"
      auth_url: "https://authentik.your-domain.com/application/o/authorize/"
      token_url: "https://authentik.your-domain.com/application/o/token/"
      userinfo_url: "https://authentik.your-domain.com/application/o/userinfo/"
      scopes: ["openid", "email", "profile"]
      auto_activate: true
```

---

## Keycloak

[Keycloak](https://www.keycloak.org/) is a popular open-source identity and access management solution.

### Step 1: Create Realm and Client

1. Log in to Keycloak admin console
2. Select your realm (or create a new one)
3. Go to **Clients** > **Create client**
4. Fill in:
   - Client ID: `vocala`
   - Client Protocol: **openid-connect**
5. On the next page:
   - Client authentication: **On**
   - Authorization: **Off**
6. Settings:
   - Valid redirect URIs: `https://YOUR_DOMAIN/auth/oauth/Keycloak/callback`
   - Web origins: `https://YOUR_DOMAIN`
7. Go to **Credentials** tab and copy the **Client Secret**

### Step 2: Configure Vocala

Replace `keycloak.your-domain.com` and `your-realm` with your values:

```yaml
oauth:
  enabled: true
  providers:
    - name: Keycloak
      client_id: "vocala"
      client_secret: "xxxxxxxxxxxxxxxxxxxx"
      auth_url: "https://keycloak.your-domain.com/realms/your-realm/protocol/openid-connect/auth"
      token_url: "https://keycloak.your-domain.com/realms/your-realm/protocol/openid-connect/token"
      userinfo_url: "https://keycloak.your-domain.com/realms/your-realm/protocol/openid-connect/userinfo"
      scopes: ["openid", "email", "profile"]
      auto_activate: true
```

---

## GitLab

Works with both GitLab.com and self-hosted GitLab.

### Step 1: Create Application

1. Go to GitLab > **User Settings** > **Applications** (or **Admin** > **Applications** for instance-wide)
2. Fill in:
   - Name: `Vocala`
   - Redirect URI: `https://YOUR_DOMAIN/auth/oauth/GitLab/callback`
   - Scopes: **openid**, **email**, **profile**, **read_user**
3. Click **Save application**
4. Copy **Application ID** and **Secret**

### Step 2: Configure Vocala

For GitLab.com:

```yaml
oauth:
  enabled: true
  providers:
    - name: GitLab
      client_id: "xxxxxxxxxxxxxxxxxxxx"
      client_secret: "xxxxxxxxxxxxxxxxxxxx"
      auth_url: "https://gitlab.com/oauth/authorize"
      token_url: "https://gitlab.com/oauth/token"
      userinfo_url: "https://gitlab.com/api/v4/user"
      scopes: ["openid", "email", "profile", "read_user"]
      auto_activate: true
```

For self-hosted GitLab, replace `gitlab.com` with your GitLab URL.

---

## Multiple Providers

You can configure several providers at once:

```yaml
oauth:
  enabled: true
  providers:
    - name: Google
      client_id: "..."
      client_secret: "..."
      auth_url: "https://accounts.google.com/o/oauth2/auth"
      token_url: "https://oauth2.googleapis.com/token"
      userinfo_url: "https://www.googleapis.com/oauth2/v3/userinfo"
      scopes: ["openid", "email", "profile"]
      auto_activate: true

    - name: GitHub
      client_id: "..."
      client_secret: "..."
      auth_url: "https://github.com/login/oauth/authorize"
      token_url: "https://github.com/login/oauth/access_token"
      userinfo_url: "https://api.github.com/user"
      scopes: ["user:email"]
      auto_activate: true
```

Each provider appears as a separate button on the login page.

---

## Troubleshooting

### "OAuth authentication failed"
- Check that `client_id` and `client_secret` are correct
- Verify the callback URL in provider settings matches exactly: `https://YOUR_DOMAIN/auth/oauth/PROVIDER_NAME/callback`
- Check server logs: `journalctl -u vocala -f`

### "Account pending activation"
- `auto_activate` is `false` -- admin needs to activate the user in admin panel
- Or set `auto_activate: true` in config

### User created with wrong name
- Display name is taken from `name` field in userinfo response (or `login` for GitHub)
- Admin can rename the user in admin panel (planned feature)

### OAuth + local login
- Users can have both OAuth and local password (if linked by email)
- OAuth-only users (created via OAuth) have empty password and can only log in via OAuth

---

## Русский

Vocala поддерживает внешнюю авторизацию через OAuth2 и OpenID Connect (OIDC). Пользователи могут входить через Google, GitHub, Keycloak, Authentik, GitLab или любой OIDC-совместимый провайдер.

### Как работает

1. Пользователь нажимает "Sign in with Google" на странице логина
2. Браузер перенаправляет на страницу авторизации провайдера
3. Пользователь подтверждает доступ
4. Провайдер перенаправляет обратно в Vocala с кодом авторизации
5. Vocala обменивает код на токен и получает информацию о пользователе (email, имя)
6. Если пользователь уже есть (по OAuth ID или email) -- выполняется вход
7. Если пользователь новый -- автоматически создаётся аккаунт

### Общая конфигурация

В `config.yaml`:

```yaml
oauth:
  enabled: true
  providers:
    - name: ИмяПровайдера       # Отображается на кнопке логина
      client_id: "xxx"           # Из панели разработчика провайдера
      client_secret: "xxx"
      auth_url: "https://..."    # Эндпоинт авторизации
      token_url: "https://..."   # Эндпоинт токена
      userinfo_url: "https://..." # Эндпоинт информации о пользователе
      scopes: ["openid", "email", "profile"]
      auto_activate: true        # Автоматически активировать OAuth пользователей
```

**Callback URL** для всех провайдеров: `https://ВАШ_ДОМЕН/auth/oauth/ИМЯ_ПРОВАЙДЕРА/callback`

### Важные моменты

- `name` в конфиге должен совпадать с callback URL (с учётом регистра)
- `auto_activate: true` -- новые OAuth пользователи сразу активны
- `auto_activate: false` -- нужна активация администратором
- Если email OAuth пользователя совпадает с существующим аккаунтом -- аккаунты связываются
- OAuth пользователи не имеют пароля -- входят только через провайдер
- Первый пользователь (локальный или OAuth) автоматически становится админом

### Настройка провайдеров

Подробные инструкции для каждого провайдера описаны выше на английском языке (Google, GitHub, Authentik, Keycloak, GitLab). Основные шаги:

1. Зарегистрировать приложение в панели провайдера
2. Указать Redirect URI: `https://ВАШ_ДОМЕН/auth/oauth/ИмяПровайдера/callback`
3. Скопировать Client ID и Client Secret
4. Добавить в `config.yaml`
5. Перезапустить Vocala: `systemctl restart vocala`

### Несколько провайдеров

Можно настроить несколько провайдеров одновременно -- каждый отображается отдельной кнопкой на странице логина.

### Решение проблем

- **"OAuth authentication failed"** -- проверьте client_id, client_secret и callback URL
- **"Account pending activation"** -- установите `auto_activate: true` или активируйте через админ-панель
- Логи: `journalctl -u vocala -f`
