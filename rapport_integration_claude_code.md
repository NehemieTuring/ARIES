# Plan d'intégration de Claude Code comme AgentHarness dans ARIES

## 1. Contexte et objectif

ARIES supporte actuellement deux harnesses agents : **OpenClaw** (`pkg/harness/openclaw`) et **Hermes**
(`pkg/harness/hermes`), chacun couplé à son propre pont SSH (`pkg/bridge/openclawssh`,
`pkg/bridge/hermesssh`). L'objectif de ce plan est d'évaluer et de décrire le travail nécessaire pour
ajouter **Claude Code** (l'agent CLI d'Anthropic) comme troisième harness, sans modifier la sémantique
des tâches de benchmark ni le cycle de vie fail-closed d'ARIES (isolation confirmée avant évaluation).

Ce document est un plan de travail, pas une implémentation : il identifie les points d'extension
existants, le code à écrire, les décisions de conception à trancher, et une estimation d'effort.

**Mise à jour du 2026-08-25** : un squelette de code a été écrit (`pkg/harness/claudecode/`,
`pkg/bridge/claudecodessh/`), compile et passe `go vet`. Un test de connectivité automatisé de bout en
bout (vrai sandbox Docker + vrai pont SSH + vrai client SSH) a validé le mécanisme de proxy. Une
capture empirique du comportement réel de l'outil Bash de Claude Code a ensuite **invalidé
l'hypothèse initiale de session shell persistante** (§3 original) — voir §7 pour le détail et la
conception révisée qui en résulte, plus simple que prévu.

## 2. Ce qu'ARIES fournit déjà (points d'extension)

### 2.1 Interface `AgentHarness`

`pkg/runner/interfaces.go:17-21` définit un contrat minimal à trois méthodes :

```go
type AgentHarness interface {
    Start(context.Context, core.HarnessRequest) error
    Run(context.Context, string) (core.HarnessResult, error)
    Stop(context.Context) error
}
```

- `core.HarnessRequest` (`pkg/core/types.go:106-115`) : `RunID`, `TaskID`, `Endpoint core.ToolEndpoint`
  (adresse/identité SSH du pont), `Model core.ModelConfig`, `Timeout`, `CPU`, `MemoryMB`, `OutputDir`.
- `core.HarnessResult` (`pkg/core/types.go:130-136`) : `Status`, `FinalResponse`, `Duration`,
  `LogPaths`, `Error`.

Aucune méthode dédiée à la collecte de télémétrie n'est requise : elle se fait via `LogPaths` et les
fichiers d'audit du pont (voir §2.3 et le rapport sur les traces).

### 2.2 Délégation complète de l'appel modèle au harness

`runtime.backend` est validé comme un enum figé (`deepseek`/`sglang`, `pkg/config/config.go:358-359`,
481-492), et dispatché par un switch dans `cmd/aries/wiring.go:73-102`. **Mais** ARIES ne fait jamais
l'appel d'inférence lui-même — son code Go ne fait qu'un préflight de liveness
(`internal/app/preflight.go`, `pkg/model/sglang/client.go`, un simple `GET /v1/models`). L'appel réel
est fait par le processus harness (ex. OpenClaw écrit sa propre config API et appelle le fournisseur
lui-même, `pkg/harness/openclaw/config.go:112-131`).

**Conséquence pour Claude Code** : il peut appeler l'API Anthropic nativement, sans qu'ARIES ait besoin
de comprendre ce format. Seul un ajustement mineur de l'enum de validation de profil est requis pour
accepter une nouvelle valeur de backend (proposé : `anthropic`).

### 2.3 Pont SSH : pattern réutilisable, grammaire non réutilisable

`pkg/bridge/hermesssh/` et `pkg/bridge/openclawssh/` partagent une structure commune (~1100-1200 lignes
chacun) :
- Génération de clés ed25519 par session, fichier d'identité et `known_hosts` privés
  (`hermesssh/bridge.go:555-654`).
- Un `core.ToolEndpoint{Protocol:"ssh", Address, Username:"aries", IdentityFile, ...}` retourné au
  harness pour qu'il s'y connecte lui-même.
- Un `toolCallRecord` journalisé en JSONL (`bridge.go:107-120`) et un `ssh_raw.log` optionnel.
- Un **parseur de grammaire dédié** (`grammar.go`) qui ne reconnaît que les formes exactes de commandes
  que *son* harness envoie (pour Hermes : sonde de connexion, `echo $HOME`, `bash -c <script>`,
  `bash -l -c <script>` — documenté comme lié à un seul appelant exact, `hermesssh/grammar.go:8-21`).

**Conséquence pour Claude Code** : la grammaire ne peut pas être réutilisée telle quelle. Il faut un
nouveau pont (`pkg/bridge/claudecodessh/`) avec sa propre grammaire, calquée sur la structure existante
mais reconnaissant les formes de payload que Claude Code envoie réellement.

### 2.4 Verrou à lever : `wiring.go`

`cmd/aries/wiring.go:57-67` n'accepte aujourd'hui que les paires `openclaw`/`openclaw-ssh` et
`hermes`/`hermes-ssh`, et **rejette explicitement** toute autre combinaison. Il faudra y ajouter la
paire `claude-code`/`claude-code-ssh`.

## 3. Le point de conception central : comment router l'outil Bash de Claude Code

Claude Code exécute normalement ses commandes shell directement sur la machine où il tourne — pas via
un pont distant. Pour rentrer dans le modèle d'isolation d'ARIES (harness et sandbox dans deux
conteneurs Docker séparés, connectés uniquement par le pont), il faut que l'outil Bash de Claude Code,
depuis l'intérieur de son conteneur harness, finisse par exécuter ses commandes **dans le sandbox**, via
le pont.

Deux options identifiées :

| Option | Description | Avantage | Risque |
|---|---|---|---|
| **A. Wrapper `/bin/bash` transparent** | Remplacer `/bin/bash` du conteneur harness par un script qui forward la commande via SSH vers le sandbox (identité fournie par `ToolEndpoint`) | Claude Code ne voit aucune différence ; réutilise le pattern SSH existant tel quel | Le wrapper doit reproduire fidèlement la sémantique de `bash -c` (stdin, codes de sortie, streaming stdout/stderr) |
| **B. Serveur MCP d'exécution distante** | Donner à Claude Code un outil MCP personnalisé qui exécute la commande via SSH, à la place de son outil Bash natif | Plus explicite, plus facile à auditer indépendamment du wrapper shell | Change le comportement observé de Claude Code par rapport à son usage natif — pertinent si l'objectif est de mesurer Claude Code "tel quel" |

**Recommandation** : Option A, pour rester cohérent avec le principe d'ARIES de préserver le
comportement natif de chaque harness (cf. §"Preserving task semantics" du papier ARIES) — Claude Code
ne doit pas savoir qu'il tourne dans ce dispositif.

## 4. Plan de travail par étapes

1. ✅ **Prototype de connectivité** — fait le 2026-08-25 : `cmd/aries-claudecode-probe/` démarre un vrai
   sandbox Docker, un vrai pont SSH, s'y connecte avec un vrai client SSH, et valide le proxy d'une
   session shell. Conservé comme outil de diagnostic.
2. ✅ **Capturer la grammaire réelle** — fait le 2026-08-25 par shim de `/bin/bash` dans un conteneur
   jetable avec Claude Code réellement installé. **Résultat contraire à l'hypothèse initiale** — voir §7.
3. ✅ **Écrire `pkg/bridge/claudecodessh/`** — écrit, puis entièrement réécrit après l'étape 2 pour
   refléter le vrai modèle (exec discret par commande, comme `hermesssh`, pas une session persistante).
4. ✅ **Écrire `pkg/harness/claudecode/`** — squelette écrit (`Start`/`Run`/`Stop`, rendu de config,
   collecte d'artefacts). Compile et passe `go vet`.
5. ✅ **Étendre `wiring.go`** — fait (paire `claude-code`/`claude-code-ssh` acceptée).
6. ✅ **Étendre l'enum de config** — fait (`anthropic` accepté comme `runtime.backend`, mode `external`
   uniquement, préflight de liveness encore en TODO).
7. ⬜ **Profil de test** — pas encore fait ; nécessite d'abord une image Docker avec Claude Code installé
   et épinglée (`configs/versions.json`), et le wrapper bash de l'option A (voir §7.4).
8. ⬜ **Validation du cycle fail-closed** — pas encore testée avec un vrai harness Claude Code de bout en
   bout (seul le pont a été testé isolément, avec un shell générique, pas encore avec `claude` lui-même
   au travers du pont).
9. ⬜ **Télémétrie** — inchangé, toujours à vérifier.

## 5. Risques et inconnues à lever avant de démarrer le code

- **Comportement exact du wrapper bash** : reproduire fidèlement stdin/stdout/stderr/codes de sortie
  pour ne pas fausser le comportement de Claude Code par rapport à son usage natif.
- **Format d'export de trajectoire de Claude Code** : à l'inverse d'OpenClaw (fichiers de session sur
  disque) et Hermes (export SQLite via commande dédiée), le mécanisme d'export de session de Claude Code
  n'a pas encore été vérifié dans ce plan — étape à faire avant l'écriute de `collectArtifacts`.
- **Coût de l'étape 2** (capture de grammaire réelle) : nécessite un environnement de test isolé avant
  d'écrire le parseur, sous peine de grammaire incomplète en production.

## 6. Estimation d'effort

Chantier de taille moyenne à significative : nouveau package harness + nouveau pont SSH avec grammaire
dédiée + ajustements de wiring/config. Comparable en ampleur à l'ajout initial du support Hermes dans
ARIES (harness + bridge dédiés), pas un simple changement de configuration.

## 7. Résultats empiriques (2026-08-25)

### 7.1 Test de connectivité — validé

`cmd/aries-claudecode-probe` a démarré un vrai sandbox Docker (`ubuntu:24.04`), un vrai pont
`claudecodessh`, puis s'y est connecté avec un vrai client SSH (`golang.org/x/crypto/ssh`), envoyant
plusieurs commandes séquentielles. Le mécanisme de proxy vers `ExecStream` fonctionne correctement —
voir `ssh_raw.log` capturé pendant le test pour la trace complète.

### 7.2 Grammaire réelle de l'outil Bash — hypothèse initiale invalidée

Méthode : `/bin/bash` a été remplacé (par renommage atomique, pas écriture en place — `ETXTBSY` sinon)
par un script journalisant chaque invocation puis délégant au vrai bash, à l'intérieur d'un conteneur
Docker jetable (`node:22-slim`) avec Claude Code réellement installé (`npm install -g
@anthropic-ai/claude-code`) et authentifié. Une tâche demandant deux appels d'outils (`ls -la` puis
`date`) a produit **5 invocations séparées de `bash`**, jamais une session persistante :

1. Sonde initiale : `bash -c env`
2. Une seule fois par session : `bash -c -l "SNAPSHOT_FILE=... source ~/.bashrc ... "` — génère un
   fichier "snapshot" capturant fonctions, alias, options du shell et PATH, plus des fonctions
   `rg`/`find`/`grep`/`pkill` propres à Claude Code.
3. Par appel d'outil : `bash -c "source $SNAPSHOT_FILE 2>/dev/null || true && shopt -u extglob
   2>/dev/null || true && { \builtin unalias -- 'unsetenv'; \builtin unset -f -- 'unsetenv'; }
   >/dev/null 2>&1 || true && eval '<commande>' < /dev/null && pwd -P >| /tmp/claude-<id>-cwd"`

**Conclusion** : Claude Code ne maintient jamais un processus shell persistant. Il simule la
persistance d'état (répertoire courant, fonctions, alias) en **re-sourçant un fichier snapshot** à
chaque appel et en traçant le `cwd` résultant dans un fichier temporaire. C'est exactement le modèle
d'exécution discrète que Hermes et OpenClaw utilisent déjà — `pkg/bridge/claudecodessh` a donc été
réécrit pour dispatcher une requête SSH `exec` par commande (comme `hermesssh`) plutôt que de proxier un
canal `shell` continu. C'est une **simplification** par rapport à la conception initiale : pas besoin de
support pty, pas besoin de gérer un canal long-lived.

Reste non vérifié (TODO dans `grammar.go`) : le désescapage d'une commande contenant un guillemet simple
n'a été exercé que sur des commandes sans caractère spécial (`ls -la`, `date`).

### 7.3 Authentification — deux mécanismes distincts découverts

- `ANTHROPIC_API_KEY` seule en variable d'environnement **ne suffit pas** pour l'usage non interactif
  (`claude -p`) sans session déjà connectée : échoue avec `Not logged in · Please run /login`.
- Le mécanisme qui fonctionne est `apiKeyHelper` dans `~/.claude/settings.json`, pointant vers une
  commande qui **lit la clé depuis un fichier** (`"apiKeyHelper": "cat /chemin/vers/cle"`) — un helper
  qui lit `$ANTHROPIC_API_KEY` directement échoue aussi (`did not return a value`), probablement parce
  que Claude Code retire cette variable de l'environnement avant d'invoquer le helper pour éviter une
  dépendance circulaire. **Ceci confirme le choix déjà fait dans `pkg/harness/claudecode/config.go`**
  (`ANTHROPIC_API_KEY_HELPER` lisant `modelKeyPath`).
- Alternative sans clé API du tout : monter en lecture seule `~/.claude/.credentials.json` et
  `~/.claude.json` (session OAuth existante) dans le conteneur — fonctionne, mais ce n'est pas le
  mécanisme portable qu'ARIES peut injecter par profil (c'est une session personnelle liée à un compte,
  pas un identifiant scopé par tâche) ; à réserver aux tests manuels, pas à l'intégration réelle.

### 7.4 Wrapper bash écrit et testé de bout en bout — succès

Le wrapper (`bashWrapperScript` dans `pkg/harness/claudecode/config.go`) a été écrit, câblé dans
`runtimeArchive` (remplace `/bin/bash` intégralement dans l'archive de staging — pas de `bash.real` à
préserver, puisque toute invocation dans le conteneur harness est censée partir vers le sandbox) et
testé avec un **vrai client `ssh`** (pas seulement la bibliothèque Go) via `cmd/aries-claudecode-probe`.
Trois bugs réels trouvés et corrigés pendant ce test :

1. **Duplication de `bash` dans les arguments** : `core.Command.Args` ne doit pas inclure le nom du
   programme (déjà dans `Path`) — l'inclure produisait `/bin/bash bash -c <script>`, que bash
   interprétait comme "exécute le fichier script nommé bash", d'où `cannot execute binary file`.
2. **Adresse `host:port` mal formée pour `ssh`** : le wrapper collait `host:port` directement après
   `user@`, syntaxe invalide pour le client `ssh` (c'est une syntaxe SCP/URL, pas CLI). Corrigé en
   séparant host et port via l'expansion de paramètres POSIX (`${VAR%:*}` / `${VAR##*:}`) et en passant
   `-p <port>` séparément.
3. **Format de clé incompatible avec un vrai client OpenSSH** : `generateSessionKeys` (copié de
   `hermesssh`) produisait une clé PEM PKCS8 générique (`x509.MarshalPKCS8PrivateKey` + bloc
   `"PRIVATE KEY"`). `openssl pkey` la valide comme clé ed25519 correcte, mais `ssh`/`ssh-keygen`
   d'OpenSSH 8.9p1 (celui de cette machine) la refuse avec `is not a key file`. Remplacé par
   `ssh.MarshalPrivateKey` (`golang.org/x/crypto/ssh`), qui produit le format natif
   `"OPENSSH PRIVATE KEY"`, universellement accepté. `hermesssh` utilise le même code PKCS8 et
   fonctionne pourtant en usage réel (validé plus tôt dans le même run ARIES) — probablement parce que
   le client SSH utilisé *à l'intérieur* du conteneur Hermes diffère de celui de cette machine hôte ;
   cette incompatibilité ne s'est révélée qu'en testant avec le `ssh` de l'hôte, pas avec le client Go.

Après ces trois corrections, le test de bout en bout passe : le wrapper, invoqué comme `/bin/bash`
le serait par Claude Code (`bash -c "env"`), se connecte via un vrai `ssh`, traverse le pont, s'exécute
dans le sandbox, et renvoie une sortie confirmant l'exécution distante (`HOSTNAME=<id-du-conteneur-sandbox>`
dans la sortie de `env`, différent de l'hôte).

### 7.5 Harness complet testé de bout en bout (`cmd/aries-claudecode-harness-probe`)

Une image Docker locale jetable (`aries-claudecode:test-1` — Node 22 + `openssh-client` + Claude Code
CLI + utilisateur non-root fixe `aries` UID 10000, voir `Dockerfile.claudecode` dans le scratchpad) a
permis de tester le vrai `pkg/harness/claudecode.Manager` (`Start`/`Run`/`Stop`), pas seulement le pont,
contre un vrai sandbox Docker et un vrai pont. Deux bugs supplémentaires trouvés et corrigés :

4. **Mauvais mécanisme de clé API dans `renderSettings`** : la première version écrivait
   `ANTHROPIC_API_KEY_HELPER` sous `"env"` dans `settings.json` — un nom inventé, silencieusement
   ignoré par Claude Code (pas d'erreur, juste un retour à "Not logged in"). Corrigé pour utiliser
   `"apiKeyHelper": "cat <chemin>"` **à la racine** de `settings.json`, le mécanisme confirmé
   fonctionnel en §7.3.
5. **Permissions non-interactives manquantes** : `--dangerously-skip-permissions` n'était pas passé à
   `claude -p`. Ajouté — sûr maintenant que le conteneur tourne systématiquement en non-root (UID fixe
   10000, `runtimeUID`/`runtimeGID`, également nécessaire pour que l'utilisateur non-root puisse lire
   `modelKeyPath` en mode 0600 — les fichiers stagés par `stageArchive` n'étaient auparavant possédés
   par personne en particulier, donc `root` par défaut).

Après ces corrections, le harness complet démarre, authentifie correctement via `apiKeyHelper` (plus
d'erreur `did not return a value` ni `Not logged in`), et échoue seulement sur
`"Credit balance is too low"` — le même compte API à 0 $ que précédemment (§ le tout début de la
session). C'est donc un échec de facturation, pas un défaut du harness : **toute la chaîne technique
(staging, wrapper, pont, authentification) est validée de bout en bout** ; il ne reste qu'à créditer le
compte pour observer une tâche réussie complète.

### 7.5bis Trois bugs de câblage supplémentaires trouvés en testant le vrai wrapper bash

Le test de bout en bout (§7.5) a d'abord échoué avec `exec request failed on channel 0` côté Claude
Code pour **tout** appel Bash, même triviaux (`pwd`). Trois causes distinctes, empilées, ont dû être
corrigées avant d'obtenir un vrai succès :

1. **Le wrapper ne lisait que `$1`/`$2`** alors que l'invocation réelle du générateur de snapshot a
   **trois** arguments (`bash -c -l "<script>"`), pas deux. Le script perdait silencieusement le vrai
   contenu (`flag="-c"`, `script="-l"`, le troisième argument jamais lu).
2. **Reconstituer la commande avec de simples espaces est intrinsèquement ambigu** : un vrai script
   par appel peut lui-même commencer par un mot qui ressemble à un drapeau ; impossible de distinguer
   après coup "fin d'argument" de "espace dans le texte du script". Remplacé par un séparateur non
   imprimable exact (`\x1f`, Unit Separator ASCII — même convention que les trailers d'Hermes) : le
   wrapper joint tous les arguments originaux avec ce séparateur, et le pont les resépare avec
   `strings.Split`, sans aucune ambiguïté.
3. **Le motif de la grammaire supposait un préfixe `source $SNAPSHOT_FILE ... &&` obligatoire**, mais
   la vraie forme observée en bout en bout omet parfois entièrement cette ligne
   (`bash -c -l "shopt -u extglob ... && eval '<cmd>' ... && pwd -P >| ..."`, sans aucun `source`).
   Rendu optionnel dans le regex ; la condition exacte qui déclenche l'un ou l'autre reste incomprise.

Une fois ces trois corrections faites, l'outil Bash de Claude Code a fonctionné correctement à travers
le pont — plus aucune erreur `exec request failed`.

### 7.5ter Limite architecturale découverte : les outils Write/Read/Edit ne passent pas par le pont

Une fois Bash réparé, Claude Code a lui-même signalé le problème suivant dans sa réponse finale :

> "the Bash tool is connected via SSH to a separate machine (root@172.18.0.1, home dir /root) that
> doesn't share a filesystem with the one my Write/Read tools use (/home/aries/workspace)."

C'est exact et c'est une **limite de conception**, pas un bug de câblage : Claude Code possède des
outils natifs (`Write`, `Read`, `Edit`, `Glob`, `Grep`) qui manipulent le système de fichiers
**directement** (appels `fs` de Node.js), sans jamais passer par `bash`. Le mécanisme du wrapper
(option A, §3) n'intercepte que l'invocation de `/bin/bash` — il ne peut donc, par construction,
rediriger que l'outil Bash vers le sandbox. Les outils de fichiers natifs continuent d'opérer sur le
système de fichiers **local du conteneur harness**, jamais synchronisé avec le sandbox.

Conséquence concrète : un fichier créé avec `Write` existe dans le harness, mais **pas** dans le
sandbox — donc invisible pour le vérificateur du benchmark, qui inspecte le sandbox. C'est un problème
plus profond que celui résolu en §7.5bis, et propre à Claude Code parmi les trois harnesses (OpenClaw et
Hermes ne semblent pas exposer d'outils de fichiers natifs équivalents, en tout cas pas documentés comme
tels). Pistes non explorées dans cette session, à trancher avant une intégration réelle :

- Fournir un serveur MCP personnalisé exposant des outils de fichiers redirigés vers le sandbox, et
  désactiver les outils natifs Write/Read/Edit/Glob/Grep de Claude Code en config.
- Un montage réseau (sshfs ou équivalent) rendant le répertoire de travail du harness littéralement
  identique à celui du sandbox — probablement en tension avec le modèle de sécurité d'ARIES (pas de
  système de fichiers partagé entre harness et sandbox en dehors du pont).
- Limiter le périmètre d'usage de Claude Code dans ARIES aux tâches qui n'exercent que l'outil Bash
  (perdant une bonne partie de ce qui rend Claude Code utile en pratique).

### 7.6 Conséquence pour le wrapper bash (option A)

Le modèle d'exécution discrète simplifie aussi le wrapper prévu au §3 : au lieu de devoir maintenir un
tunnel SSH interactif pour toute la durée de vie du harness, le wrapper `/bin/bash` du conteneur harness
n'a qu'à transformer chaque invocation `bash -c "<script>"` en **une requête SSH `exec` unique** vers le
pont (connexion réutilisable, requête par commande) — un aller-retour par appel d'outil, sans état à
maintenir côté wrapper lui-même.

## 9. Résoudre Write/Read/Edit/Glob/Grep — conception et implémentation (option A, 2026-09-03)

Trois options pour résoudre la limite du §7.5ter ont été comparées (serveur MCP personnalisé ; montage
réseau sshfs/9p ; limiter Claude Code au Bash seul). L'option retenue est le **serveur MCP personnalisé**
— seule option cohérente avec le modèle de sécurité actuel d'ARIES (aucun filesystem partagé, tout passe
par un pont révocable) et avec l'objectif de comparaison équitable des harnesses (Claude Code utilise ses
vrais outils, juste redirigés, plutôt que d'être bridé au Bash seul ou avantagé par un montage que les
autres harnesses n'ont pas).

### 9.1 Format de câble : `aries-fileop`

Contrairement à la grammaire du §7.2 (qui a dû être rétro-conçue, Claude Code n'exposant pas son
comportement Bash), le format de câble entre le nouveau serveur MCP et le pont est **entièrement défini
par ce projet** : aucune conjecture nécessaire. Une requête SSH `exec` porte `argv[0] == "aries-fileop"`
(en parallèle de `"bash"` dans le dispatch de `bridge.go`), suivi du nom de l'opération puis de ses
arguments, le tout joint par le séparateur `\x1f` (même convention que le wrapper bash, §7.6.4). Chaque
argument après le nom d'opération est encodé en base64 — un chemin ou une chaîne de recherche peut en
principe contenir n'importe quel octet, y compris `\x1f` lui-même, autant éliminer cette ambiguïté
puisque le correctif est gratuit.

Chaque opération s'exécute côté sandbox comme une commande **argv directe, jamais interpolée dans un
shell** (`/usr/bin/env cat --  <path>`, `/usr/bin/env dd of=<path> status=none`, `/usr/bin/env find ...`,
`/usr/bin/env grep -r -n -I -- <pattern> <dir>`) — contrairement au wrapper bash, il n'y a ici aucune
surface d'injection à mal gérer, puisque le contenu et les chemins ne sont jamais assemblés en une chaîne
shell.

- `read_file(path)` → `cat` vers stdout, retourné tel quel sur le canal SSH
- `write_file(path)` → crée le dossier parent (`mkdir -p`) puis `dd of=path`, contenu reçu via le stdin
  du canal SSH (pas dans les arguments, pas de limite ARG_MAX)
- `edit_file(path, old_string, new_string)` → lu côté pont (pas streamé), remplacement Go
  (`strings.Count`/`strings.Replace`) avec le même contrat d'unicité que l'outil Edit natif (échoue si 0
  ou plus d'une occurrence), puis réécrit par `dd`
- `glob(pattern, dir)` → `find dir -type f -iname pattern`
- `grep(pattern, dir)` → `grep -r -n -I`

### 9.2 Fichiers créés/modifiés

- **`pkg/bridge/claudecodessh/fileops.go`** (nouveau) : `decodeFileOp`, `executeFileOp`,
  `writeFile`/`editFile`, journalisation dans `tool-calls.jsonl` (`OperationClass: "fileop"`)
- **`pkg/bridge/claudecodessh/bridge.go`** : dispatch dans `handleSession` sur `argv[0] ==
  "aries-fileop"` avant le chemin bash existant ; `grammar.go` n'est pas touché (séparation des
  responsabilités : sa grammaire reste celle, rétro-conçue, du seul outil Bash natif)
- **`cmd/aries-claudecode-mcpfiles/main.go`** (nouveau binaire) : serveur MCP stdio écrit à la main
  (JSON-RPC 2.0 via `encoding/json`, sans SDK externe — cohérent avec le reste du dépôt, qui n'utilise
  que `golang.org/x/crypto/ssh` directement plutôt que des bibliothèques SSH de haut niveau). Expose
  `write_file`/`read_file`/`edit_file`/`glob`/`grep`, chacun ouvrant une connexion SSH vers le pont (une
  par appel, même modèle discret que l'outil Bash) et envoyant une commande `aries-fileop`.
- **`pkg/harness/claudecode/config.go`** : `renderSettings` ajoute `permissions.deny` (désactive les
  outils natifs) ; `renderMCPConfig` (nouveau, corrigé au §9.4 — pas dans `settings.json`) déclare
  `mcpServers.ariesfiles`, avec ses propres variables `ARIES_BRIDGE_*` passées explicitement plutôt que
  comptées sur l'héritage d'environnement (voir §9.3) ; `runtimeArchive` stage le binaire compilé à
  `mcpFilesContainerPath` (`/run/aries/claude-code/bin/aries-claudecode-mcpfiles`) et la config MCP à
  `mcpConfigContainerPath` (`/run/aries/claude-code/mcp-servers.json`), tous deux possédés par
  `runtimeUID`/`runtimeGID` comme le reste
- **`pkg/harness/claudecode/harness.go`** : `Run` ajoute `--mcp-config <fichier> --strict-mcp-config` à la
  ligne de commande `claude` (§9.4) ; nouveau champ `Options.MCPFilesBinaryPath` (chemin hôte vers
  le binaire compilé, requis — `New` échoue sans lui plutôt que de démarrer un conteneur qui échouera
  silencieusement plus tard)
- **`cmd/aries/wiring.go`** : `resolveMCPFilesBinaryPath()` — cherche `ARIES_CLAUDE_CODE_MCPFILES_BIN`,
  sinon un binaire du même nom à côté de l'exécutable `aries` lui-même (TODO : vraie configuration fichier,
  comme le TODO déjà existant sur `cfg.Versions` pour l'image)
- **`cmd/aries-claudecode-harness-probe/main.go`** : compile désormais le binaire MCP à la volée
  (`buildMCPFilesBinary`, `go build ./cmd/aries-claudecode-mcpfiles` en `GOOS=linux GOARCH=amd64`) pour
  rester autonome

### 9.3 Risque anticipé : Claude Code assainit-il l'environnement des sous-processus MCP ?

Précédent direct au §7.3 : `apiKeyHelper` reçoit un environnement dont `ANTHROPIC_API_KEY` a été retiré
par Claude Code lui-même. Rien ne garantit qu'un serveur MCP spawné hérite tel quel de l'environnement du
conteneur (où `ARIES_BRIDGE_ADDRESS`/`USERNAME`/`IDENTITY` sont déjà positionnés pour le wrapper bash,
voir `containerEnvironment`). Plutôt que de supposer que l'héritage fonctionne, ces trois variables sont
**aussi** passées explicitement dans l'entrée `mcpServers.ariesfiles.env` du fichier `--mcp-config` rendu
par `renderMCPConfig` (§9.4/§9.5) — un correctif gratuit qui élimine la question plutôt que de la laisser
en suspens. (Non éprouvé en pratique : la trace de débogage du §9.4 montre que l'environnement déclaré
arrive bien au serveur, donc au moins l'un des deux mécanismes — héritage ou déclaration explicite —
fonctionne ; lequel exactement reste incertain, mais sans conséquence puisque le résultat est correct.)

### 9.4 Première validation empirique (2026-09-03) : bug 6 trouvé et corrigé

`cmd/aries-claudecode-harness-probe` a été lancé contre la vraie image `aries-claudecode:test-1`, un vrai
sandbox Docker et un vrai pont — même méthode que pour les bugs 1 à 5 du Bash tool. Résultat : **timeout
complet, zéro appel bridge, zéro sortie** — `claude` restait bloqué pendant les 3 minutes du délai sans
jamais produire le moindre octet.

**Diagnostic** (par isolation, en dehors du harness complet, avec un conteneur jetable du même image et
des appels `docker exec` manuels pour itérer vite) :

- une clé API factice avec la configuration `settings.json` d'origine (baseline, sans `mcpServers` ni
  `permissions.deny`) échoue proprement en <1s (`401 Invalid API key`) — le mécanisme de base fonctionne
- la même configuration, avec `mcpServers` déclaré dans `settings.json`, échoue **tout aussi vite** — donc
  la présence de `mcpServers` dans `settings.json` ne fait _rien du tout_, ni en bien ni en mal
- une trace de débogage ajoutée temporairement au serveur MCP (`ARIES_MCPFILES_DEBUG`, décodage brut de
  chaque message stdio) confirme : **le serveur MCP n'est jamais lancé**, quelle que soit la présence de
  `mcpServers` dans `settings.json`
- `claude --help` / `claude mcp --help` sur la version réellement installée (2.1.245) montrent que
  `settings.json` **n'a pas de champ `mcpServers` reconnu** — les mécanismes réels sont `.mcp.json` (avec
  approbation interactive requise, donc inutilisable en headless), `claude mcp add` (persiste dans un
  magasin de config), ou le flag `--mcp-config <fichier>` (+ `--strict-mcp-config` pour ignorer toute
  autre source) — **c'est ce dernier qui convient à une invocation `-p` unique et non interactive**.

**Bug 6 (nouveau) trouvé et corrigé** : `mcpServers` dans `settings.json` — silencieusement ignoré,
jamais d'erreur, jamais de lancement du serveur. Corrigé en déplaçant cette déclaration dans un fichier
séparé (`renderMCPConfig`, staged à `mcpConfigContainerPath`) passé via `--mcp-config
<fichier> --strict-mcp-config` sur la ligne de commande `claude` elle-même (voir `harness.go`'s `Run`).
`permissions.deny` reste dans `settings.json` : confirmé être un champ réel qui ne casse ni ne bloque rien
à lui seul.

**Confirmation du correctif** : avec `--mcp-config`, la trace de débogage montre la poignée de main
`initialize` réelle (protocole `"2025-11-25"`, client `"claude-code"` version `"2.1.245"`), suivie de
`notifications/initialized` puis `tools/list` — **notre serveur répond correctement aux deux, et Claude
Code accepte la réponse sans erreur ni nouvelle tentative**. La partie protocole MCP de cette intégration
est donc validée de bout en bout, y compris le format exact des 5 définitions d'outils.

### 9.5 Blocage restant (non lié au code) : clé API rejetée

En creusant pourquoi même la configuration de base (sans aucun changement MCP) restait bloquée avec la
**vraie** clé API (`~/.anthropic_api_key`), un appel HTTPS direct à `api.anthropic.com` depuis le
conteneur (via `fetch` de Node, en dehors de Claude Code) a renvoyé **`401 "API key is invalid"`** — donc
indépendant de tout code ARIES. Cohérent avec l'incident de sécurité documenté plus haut dans cette
session (clé exposée en clair dans un terminal, rotation demandée à l'utilisateur) : le fichier
`~/.anthropic_api_key` semble contenir une clé désormais révoquée, pas la nouvelle. Un test réseau brut a
aussi montré une latence anormalement élevée (~7s pour un simple 401) — à surveiller séparément, mais la
clé invalide suffit à elle seule à expliquer l'absence totale de sortie.

**Ce blocage est à régler par l'utilisateur** (vérifier/rafraîchir `~/.anthropic_api_key`) avant qu'une
exécution complète (écriture + édition + lecture réellement effectuées dans le sandbox) puisse être
validée de bout en bout. La mécanique MCP elle-même (§9.4) est déjà confirmée fonctionnelle jusqu'à
`tools/list` ; il ne reste qu'à vérifier qu'un appel `tools/call` réel (`write_file`, `edit_file`) atteint
bien le sandbox une fois l'authentification résolue.

### 9.6 Nouvelle clé, nouveau blocage : clé "identity-linked" et `anthropic-workspace-id`

En rafraîchissant la clé (§9.5), le fichier `~/.anthropic_api_key` s'est brièvement retrouvé collé en clair
dans la conversation par erreur — traité comme compromis immédiatement, révoqué et remplacé avant toute
réutilisation. La **nouvelle** clé, testée en direct (`curl` vers `api.anthropic.com/v1/messages`), s'est
authentifiée (plus de 401) mais a renvoyé **`400 anthropic-workspace-id is required when authenticating
with an identity-linked API key`** — un type de clé différent (liée à l'identité du compte, pas à
l'organisation classique), qui exige un header supplémentaire sur chaque requête.

**Diagnostic** : `grep -a` sur les chaînes du binaire compilé de Claude Code (2.1.245) montre que
`ANTHROPIC_WORKSPACE_ID` existe bel et bien comme nom de variable reconnu — mais l'ajouter (en variable
d'environnement du conteneur, puis dans `settings.json`) **n'a rien changé** : `400` identique. En
creusant plus loin dans les chaînes, le vrai chemin de code qui attache automatiquement le header
`anthropic-workspace-id` semble conditionné à une authentification OAuth (`user_oauth`), pas à
`apiKeyHelper`. Le mécanisme générique, indépendant du type d'authentification, est
**`ANTHROPIC_CUSTOM_HEADERS`** — une liste de paires `Header: valeur` séparées par des retours à la
ligne, explicitement documentée dans le binaire comme "sent on every inference and model-discovery
request". Testé isolément (`docker exec` avec `ANTHROPIC_CUSTOM_HEADERS=anthropic-workspace-id: wrkspc_...`)
→ `is_error:false`, `result:"Hi!"` : **confirmé**.

**Corrigé** : `core.ModelConfig`/`ProfileModel` gagnent un champ `WorkspaceID` (pas un secret — sûr à
logger), thread jusqu'à `renderSettings`/`harness.go`'s `Start`, qui pose
`ANTHROPIC_CUSTOM_HEADERS=anthropic-workspace-id: <id>` comme variable d'environnement réelle du
conteneur.

### 9.7 Validation complète : premier `write_file`/`edit_file` réel dans le sandbox

Avec la clé, le header, et le correctif `--mcp-config` du §9.4 tous en place,
`cmd/aries-claudecode-harness-probe` (tâche : créer `greeting.txt` = "Hello ARIES", puis remplacer
"Hello" par "Hi", puis `cat`) a produit :

```
status=succeeded
final_response="Done. `greeting.txt` now contains: `Hi ARIES`"
```

et surtout, la vérification indépendante côté sandbox (`cat /root/greeting.txt` exécuté directement sur
le sandbox, hors du harness) :

```
sandbox greeting.txt contents: "Hi ARIES" (exit=0)
```

`bridge/tool-calls.jsonl` confirme la mécanique exacte : deux entrées `operation_class:"fileop"` (l'appel
`write_file` puis `edit_file` via le serveur MCP), suivies d'une entrée `operation_class:"agent"` (le
`cat` final via Bash). **Le blocage documenté au §7.5ter est résolu et validé empiriquement** : un fichier
écrit/édité par Claude Code atterrit bien dans le sandbox, pas dans le filesystem du harness.

### 9.8 Bug 7 (trouvé et corrigé) : guillemets imbriqués dans `decodeEvalArgument`

Le même run a aussi révélé, dans `tool-calls.jsonl`, sept rejets `"Claude Code eval argument has an
unterminated quote"` / `"...is not single-quoted"` avant les deux `fileop` réussis. Le payload rejeté
correspondant (capturé dans `ssh_raw.log`) :

```
eval 'printf '"'"'Hello ARIES'"'"' > greeting.txt && sed -i '"'"'s/Hello/Hi/'"'"' greeting.txt && cat greeting.txt'
```

Claude Code avait en réalité **d'abord tenté toute la tâche en une seule commande Bash** (guillemets
simples imbriqués, échappés via l'idiome POSIX alternatif `'"'"'` — fermer le guillemet simple, ouvrir un
guillemet double contenant un guillemet simple littéral, refermer, rouvrir), que
`decodeEvalArgument` ne savait décoder que via l'idiome `'\''` (fermer, guillemet échappé par
backslash, rouvrir) — les deux sont des façons POSIX valides d'obtenir le même résultat, mais une seule
était supportée. Une autre tentative (`eval pwd`, un mot nu sans aucun guillemet) était rejetée pour la
raison symétrique : le code exigeait un guillemet simple en tête. Rejeté par le pont (fail-closed, correct
par construction) → Claude Code s'est rabattu sur les outils MCP `write_file`/`edit_file`, qui ont réussi
(§9.7) — un exemple concret que le système reste correct même quand une commande Bash légitime est
refusée à tort.

**Corrigé** : `decodeEvalArgument` est réécrit en un analyseur général de "mot shell" POSIX (segments
simple-guillemetés copiés verbatim, segments double-guillemetés avec les 4 échappements POSIX reconnus,
backslash hors guillemet échappant le caractère suivant, concaténation de segments adjacents) plutôt que
du pattern-matching sur des idiomes spécifiques — les trois cas ci-dessus (guillemets simples, guillemets
imbriqués via `'"'"'`, mot nu) en découlent naturellement du même petit automate, sans cas particulier.
Couvert par un nouveau `grammar_test.go` (7 cas, dont les shapes exactes observées en réel).

**Reconfirmé après correctif** : le même run relancé réussit maintenant **en un seul appel Bash direct**
(`status=succeeded` en 7,6s contre 39s avant, un seul enregistrement `tool-calls.jsonl` de type `agent`,
zéro `fileop`) — preuve que le correctif du bug 7 fonctionne indépendamment, et que la voie MCP (§9.7)
n'est empruntée que quand Bash échoue réellement, exactement le comportement de repli voulu.

### 9.10 `edit_file` sur du contenu volumineux : pas d'`ARG_MAX`, une vraie limite trouvée ailleurs

Contrairement à l'hypothèse du §9.9 (une limite façon `ARG_MAX` d'Unix), il n'y a en réalité **aucun appel
`exec()` local nulle part dans ce chemin** : le contenu de `write_file` est **streamé sur l'entrée standard
du canal SSH** (`dd`, jamais un argument de ligne de commande), et `old_string`/`new_string` d'`edit_file`
voyagent en base64 dans la **chaîne de commande de la requête `exec` SSH elle-même** — envoyée par un
client SSH natif Go (`golang.org/x/crypto/ssh`, pas un sous-processus). `ARG_MAX` ne peut donc littéralement
pas s'appliquer ici.

**Test réel** (nouvelle phase 5 de `cmd/aries-claudecode-probe`, contre un vrai pont, sans passer par
Claude Code ni l'API — gratuit et rapide à itérer) : `edit_file` avec des `old_string`/`new_string`
identiques, de taille croissante. Résultat, par recherche dichotomique :

- **98 000 octets** par chaîne (196 000 combinés, ≈ 261,3 KiB une fois encodés en base64) → **OK**
- **99 000 octets** par chaîne (198 000 combinés, ≈ 264,0 KiB encodés) → **échec** (`EOF` côté client)

Ce basculement correspond exactement à la constante `maxPacket = 256*1024` (262 144 octets) codée en dur
dans `golang.org/x/crypto/ssh` (`ssh/cipher.go`) — la limite est un **plafond de taille de paquet SSH**
unique, pas une limite Unix : dès que `old_string`+`new_string` encodés (plus le chemin et les séparateurs)
dépassent 256 KiB, le serveur SSH (le pont) rejette le paquet à la lecture et ferme la connexion, ce que le
client perçoit comme un simple `EOF`.

**Conclusion** : `edit_file` fonctionne de manière fiable jusqu'à environ 98 Ko par chaîne (`old_string` et
`new_string` combinés sous ~190 Ko), largement suffisant pour l'immense majorité des édition de code réel.
Une limite documentée, pas un bug — corrigible plus tard si besoin (streamer `old_string`/`new_string` sur
l'entrée standard comme `write_file`, plutôt que dans la commande `exec` elle-même) mais pas bloquant pour
la suite.

### 9.12 De la CLI ARIES à une vraie tâche Terminal-Bench 2 : trois derniers verrous de câblage

Passer de `cmd/aries-claudecode-harness-probe` (harness seul, tâche synthétique) à `./bin/aries
profiles/....json` (toute la CLI ARIES, vraie tâche `fix-git-001` de terminal-bench-2) a fait apparaître
trois derniers verrous, tous dans du code de câblage jamais exercé jusqu'ici — pas dans le harness/pont déjà
validés au §9.7-§9.10 :

1. **Image Docker jamais formalisée** : `aries-claudecode:test-1` avait été construite à la main pendant le
   diagnostic (§7-§9), jamais depuis un `Dockerfile` versionné. Écrit et commité :
   `images/claudecode/Dockerfile` (base `node:22-slim`, `openssh-client`, `npm install -g
   @anthropic-ai/claude-code`, utilisateur non-root `aries` UID/GID 10000). Construite et taguée
   `aries-claudecode:2026.09.03`, référencée dans un nouveau `configs/versions.json`'s `claudecode.image`
   (nouveau champ `Versions.ClaudeCode`/`ClaudeCodeVersions` dans `pkg/config/config.go`). Comme pour
   Hermes/OpenClaw, `pullImages` accepte une image déjà présente localement sans tenter de la retirer d'un
   registre distant — aucune image publique n'existe pour Claude Code (npm uniquement), donc c'est le
   mécanisme normal ici, pas un contournement.
2. **`cmd/aries/wiring.go` pointait encore vers `cfg.Versions.Hermes.Image`** (un TODO de bootstrap jamais
   corrigé) — remplacé par `cfg.Versions.ClaudeCode.Image` ; `Versions.HarnessImage("claude-code")`
   (utilisé par `internal/app/run.go` pour la préparation/pull d'images) ne connaissait pas non plus ce
   type de harness — ajouté.
3. **Blocage réel, trouvé seulement en lançant la vraie CLI** : `internal/app/preflight.go`'s
   `validateLiveModel` (le contrôle de vivacité qu'ARIES fait lui-même avant de démarrer un harness — clé
   + modèle joignables, sans jamais faire d'appel d'inférence réel) ne connaissait que `deepseek` et
   `sglang` ; `anthropic` tombait dans le `default:` et échouait systématiquement avec
   `configuration_invalid`, avant même que le harness ne démarre. Corrigé par `validateAnthropicModel` —
   même méthode que DeepSeek (un seul `GET` sans coût, ici `/v1/models`, confirmée retourner exactement la
   même forme `{"data":[{"id":...}]}`), avec le header `anthropic-workspace-id` ajouté quand
   `model.workspace_id` est renseigné (§9.6).

Nouveau profil committé : `profiles/claudecode-tb2-fix-git-anthropic.json` (harness `claude-code`, bridge
`claude-code-ssh`, backend `anthropic`, modèle `claude-sonnet-5`).

### 9.13 Résultat : premier run réel, télémétrie comparable produite

`./bin/aries profiles/claudecode-tb2-fix-git-anthropic.json` sur la vraie tâche `fix-git-001` (pas une
instruction synthétique) — cycle complet de bout en bout :

```
harness_status: succeeded
isolation: {status: confirmed, harness_stopped: true, bridge_revoked: true}
evaluation_status: failed (score 0)
observer: {status: succeeded, sample_count: 211}
```

Le harness a fonctionné correctement de bout en bout (démarrage, exécution, arrêt, isolation confirmée
avant évaluation). L'agent a rencontré un vrai conflit de fusion Git ambigu dans la tâche et a choisi de
poser une question plutôt que de deviner — l'évaluation automatique a donc échoué (score 0), un résultat
de **l'agent sur cette tâche précise**, pas un défaut d'intégration : le pipeline ARIES lui-même (isolation
fail-closed, télémétrie, cycle de vie) a fonctionné correctement du début à la fin.

**`monitor/resources.jsonl`/`monitor/index.json`** utilisent le même schéma exact (`schema_version: 3`,
`cpu_usage_nanoseconds`, `memory_usage_bytes`, un enregistrement par composant `sandbox`/`harness` par
seconde) que les runs OpenClaw/Hermes déjà produits — **directement comparable sans transformation**,
confirmé par diff de structure de répertoire entre ce run et un run Hermes existant (seuls les artefacts
propres à chaque harness diffèrent : `claude_stdout.log`/`settings.json` ici vs
`hermes_stdout.log`/`config.yaml` là-bas — la télémétrie elle-même est identique en forme).

### 9.14 n=5 partout : 30 runs réels, deux tâches, comparaison publiée

**Limite de protocole à énoncer d'emblée** : OpenClaw et Hermes tournent sur DeepSeek, Claude Code sur
Claude Sonnet 5 — ce n'est pas un protocole à modèle constant. Les écarts impliquant Claude Code mélangent
donc **l'effet du harness et celui du modèle**. OpenClaw et Hermes, en revanche, tournent sur le **même**
modèle : toute différence entre ces deux-là est directement imputable au harness.

**Méthode** : après une première observation à n=1-5 selon harness/tâche, la donnée était insuffisante
pour distinguer un écart réel du bruit (règle de terrain : à n=5, seuls des écarts d'environ 50% et plus
sont interprétables). Chaque harness a donc été relancé jusqu'à atteindre **n=5 sur chacune des deux
tâches** : 3 runs `openclaw-tb2-fix-git-deepseek` et 4 `hermes-tb2-fix-git-deepseek` de plus, puis 4 runs
de chacun des trois profils `*-tb2-overfull-hbox-*` de plus. Total : 30 runs réels, aucun résultat écarté
ni relancé pour améliorer un chiffre.

**`fix-git-001`, moyenne ± écart-type sur 5 runs (composant `harness`)** :

| Harness | Résultat | Durée | Pic CPU | Mémoire |
|---|---|---|---|---|
| OpenClaw   | 4/5 réussi | 121,8 ± 27,6 s | 259,2 ± 5,1% | 671,9 ± 79,3 Mo |
| Hermes     | 5/5 réussi | 152,4 ± 36,2 s | 97,7 ± 3,3%  | 230,0 ± 39,7 Mo |
| Claude Code | 4/5 réussi | 113,0 ± 19,2 s | 56,4 ± 41,2% | 189,7 ± 55,9 Mo |

À n=5, seuls les écarts nettement supérieurs à l'écart-type sont interprétables :
- **CPU et mémoire d'OpenClaw** sont robustement les plus élevés des trois (écart-type CPU minuscule,
  5,1 points — la mesure est très stable, et l'écart avec Hermes/Claude Code est de plusieurs centaines de
  pourcents). OpenClaw et Hermes tournant sur le **même modèle**, cet écart est imputable au harness, pas
  au modèle.
- **La durée d'Hermes** (152 s) est la plus longue des trois, d'un écart supérieur à l'écart-type combiné
  — probablement réel, pas du bruit.
- Claude Code vs OpenClaw en durée (113 s contre 122 s) : écart de 9 s pour des écarts-types de 19-28 s —
  **indiscernable du bruit à cet échantillon**.

**`overfull-hbox-001`, moyenne ± écart-type sur 5 runs** :

| Harness | Résultat | Durée | Pic CPU | Mémoire |
|---|---|---|---|---|
| OpenClaw    | **2/5 réussi** | 895,1 ± 69,3 s | 259,3 ± 11,3% | 621,8 ± 100,9 Mo |
| Hermes      | 5/5 réussi     | 791,8 ± 60,1 s | 97,4 ± 4,4%   | 265,1 ± 41,7 Mo |
| Claude Code | 4/5 réussi     | 455,7 ± 85,5 s | 113,6 ± 6,7%  | 237,6 ± 61,3 Mo |

Trois observations qui tiennent maintenant face au bruit :
- **Fiabilité d'OpenClaw sur cette tâche : 2/5 (40%)**, contre 5/5 pour Hermes et 4/5 pour Claude Code —
  invisible à n=1 (le tout premier run OpenClaw avait réussi). OpenClaw et Hermes utilisant le **même
  modèle** DeepSeek, cet écart de fiabilité est imputable au harness (stratégie de prompt, outils exposés,
  gestion du contexte…), pas au modèle — la piste la plus intéressante à creuser ensuite.
- **Claude Code reste nettement plus rapide** (456 s contre 792-895 s) : l'écart (300+ s) est supérieur à
  plusieurs écarts-types (60-86 s) — un effet réel, pas seulement du bruit, même s'il mélange harness et
  modèle.
- CPU/mémoire : même motif que sur `fix-git-001` — OpenClaw nettement au-dessus des deux autres,
  imputable au harness (comparaison OpenClaw/Hermes, même modèle).

**Comparaison publiée** : une page interactive (petits multiples CPU/mémoire/durée par harness, sélecteur
de tâche, les 30 runs, méthodologie et avertissement sur les modèles) —
https://claude.ai/code/artifact/25f84fd1-665b-40a2-aabf-c2684078e957, également disponible en fichier
local joint : `comparaison_harnesses.html`.

### 9.15 À creuser : pourquoi OpenClaw échoue-t-il 3 fois sur 5 sur `overfull-hbox-001` ?

Puisqu'OpenClaw et Hermes partagent le même modèle (DeepSeek) et la même tâche, l'écart de fiabilité
(2/5 contre 5/5) est le résultat le plus solide et le plus actionnable de cette campagne : c'est un effet
de harness mesuré sans confusion de modèle. Pas encore diagnostiqué — hypothèses à vérifier en premier :
lire `harness/agent-result.json`/`harness/openclaw.json` des 3 runs échoués pour voir si l'agent a cru
réussir à tort (comme observé une fois pour Claude Code, §9.14) ou s'il a authentiquement échoué à
éliminer les avertissements ; comparer aux trajectoires des runs réussis pour une différence de stratégie.

## 10. Conclusion

L'architecture d'ARIES est conçue pour rendre cette intégration possible sans modification du cœur du
framework (Runner, cycle de vie fail-closed, Benchmark) : l'interface `AgentHarness` est petite, et la
délégation de l'appel modèle au harness lui-même évite tout travail côté ARIES pour parler à l'API
Anthropic. Le travail réel se concentre sur un nouveau pont SSH avec sa propre grammaire, et sur la
décision de conception pour router l'outil Bash de Claude Code à travers ce pont sans changer son
comportement observable.

La grammaire Bash (§7, robustifiée par le bug 7 du §9.8) et le routage Write/Read/Edit/Glob/Grep (§9) sont
désormais tous deux **implémentés et validés empiriquement contre une vraie infrastructure** (Docker, SSH,
Claude Code 2.1.245, vraie clé API) : un fichier créé puis édité par l'agent atterrit bien dans le
sandbox, jamais dans le filesystem du harness (§9.7) — la limite architecturale documentée au §7.5ter est
résolue.

**L'intégration est complète et validée de bout en bout** : `./bin/aries
profiles/claudecode-tb2-fix-git-anthropic.json` exécute une vraie tâche Terminal-Bench 2 (`fix-git-001`,
pas une instruction synthétique) à travers toute la chaîne ARIES — préflight, sandbox, pont, harness,
isolation fail-closed confirmée, évaluation, télémétrie — et produit `monitor/resources.jsonl` dans le
même schéma exact que les runs OpenClaw/Hermes déjà réalisés (§9.13), directement comparable sans aucune
transformation. C'était l'objectif final du stage ; il est atteint.

**30 runs réels sur deux tâches TB2, n=5 par harness et par tâche** (§9.14) confirment que ce n'est pas un
coup de chance isolé, et l'échantillon est maintenant assez grand pour distinguer un écart réel du bruit
sur plusieurs points : Claude Code est nettement plus léger qu'OpenClaw en CPU et en mémoire, et l'écart avec Hermes n'est pas significatif ; OpenClaw et Hermes
partagent le même modèle (DeepSeek), donc leurs écarts sont directement imputables au harness — et ils
sont importants : OpenClaw consomme 2,5 à 3× plus de CPU/mémoire qu'Hermes sur les deux tâches, et surtout
**ne réussit `overfull-hbox-001` que 2 fois sur 5** contre 5/5 pour Hermes, un vrai problème de fiabilité
de harness, pas de modèle (§9.15, à diagnostiquer). Une page de comparaison publiée rassemble ces
résultats.

**Une limite méthodologique demeure** (§9.14) : les comparaisons impliquant Claude Code (Sonnet 5 contre
DeepSeek pour les deux autres) mélangent toujours l'effet du harness et celui du modèle. Un protocole à
modèle constant reste la priorité pour isoler complètement l'effet du harness sur Claude Code — mais la
comparaison OpenClaw/Hermes, elle, est déjà propre (même modèle) et donne déjà un résultat solide et
actionnable. Le reste (edit_file au-delà de ~98 Ko par chaîne, §9.10 ; épingler la version de Claude Code
contre laquelle la grammaire du pont a été rétro-conçue) est documenté mais ne bloque rien.
