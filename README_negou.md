# Notes de stage — Negou

Journal de mise en route d'ARIES sur ma machine (Ubuntu 22.04). Objectif : faire
tourner un premier run local du profil `openclaw-tb2-fix-git-deepseek.json`.

## 1. Installation de Go

Go n'était pas installé. `apt` ne propose que Go 1.18 (trop ancien pour ARIES,
qui exige 1.26.5). Installation manuelle depuis le tarball officiel :

```sh
curl -OL https://go.dev/dl/go1.26.5.linux-amd64.tar.gz
sudo rm -rf /usr/local/go
sudo tar -C /usr/local -xzf go1.26.5.linux-amd64.tar.gz
echo 'export PATH=$PATH:/usr/local/go/bin' >> ~/.bashrc
source ~/.bashrc
```

## 2. Clé API DeepSeek

```sh
echo 'ma_cle' > DEEPSEEK_API.key
chmod 600 DEEPSEEK_API.key
```

## 3. Build

```sh
make build
```

## 4. Problème Docker : `--storage-opt` nécessite XFS + pquota

Premier run → échec immédiat à la création du conteneur sandbox :

```
Error response from daemon: --storage-opt is supported only for overlay
over xfs with 'pquota' mount option
```

Cause : la tâche `fix-git-001` impose une limite de taille disque au conteneur
(`StorageMB` dans son `task.toml`), mais Docker n'autorise cette option que si
son `data-root` est sur un système de fichiers **XFS monté avec `pquota`**. Ma
partition racine est en **ext4** → incompatible.

Pas de partition libre pour reformater (le reste du disque est en NTFS, dual
boot Windows) → solution : un fichier-image XFS monté en boucle, dédié au
`data-root` Docker.

```sh
sudo apt install xfsprogs
sudo truncate -s 6G /var/lib/docker-xfs.img
sudo mkfs.xfs /var/lib/docker-xfs.img
sudo mkdir -p /mnt/docker-xfs
sudo mount -o loop,pquota /var/lib/docker-xfs.img /mnt/docker-xfs
sudo systemctl stop docker
echo '{"data-root": "/mnt/docker-xfs"}' | sudo tee /etc/docker/daemon.json
sudo systemctl start docker
```

⚠️ Le montage en boucle n'est pas persistant au redémarrage (pas encore ajouté
à `/etc/fstab`) — à refaire si la machine reboote.

Effet de bord : le changement de `data-root` repart d'un stockage Docker vide,
il a donc fallu retélécharger les images (`openclaw:2026.7.1`,
`fix-git:20251031`).

## 5. Espace disque

La partition racine (ext4, 130 Go) était à 97% pleine (~4 Go libres) avant
nettoyage manuel, remontée à ~8 Go libres ensuite. À surveiller : le fichier
XFS de 6 Go dédié à Docker laisse peu de marge pour des profils
multi-tâches.

## 6. Résultat du run test

Après la correction XFS, la chaîne complète fonctionne :

- ✅ build, préparation (checkout Terminal-Bench + pull images)
- ✅ préflight modèle DeepSeek (`runtime_state: healthy`)
- ✅ création du sandbox Docker + pont SSH OpenClaw
- ✅ harness OpenClaw démarré
- ✅ isolation confirmée (harness arrêté, bridge révoqué)
- ✅ cleanup réussi
- ❌ harness et évaluation en échec :

```
FailoverError: aries (deepseek-v4-flash) returned a billing error —
your API key has run out of credits or has an insufficient balance.
```

Vérification sur le dashboard DeepSeek (`platform.deepseek.com` → Billing) :
`Topped-up: 0`, `Granted: 0`, aucune facture — le compte n'a jamais été
crédité, pas de crédit gratuit accordé.

## Prochaines étapes

- [ ] Créditer le compte DeepSeek (onglet "Top up") puis relancer
      `./bin/aries profiles/openclaw-tb2-fix-git-deepseek.json`
- [ ] Rendre le montage XFS persistant (`/etc/fstab`) si la machine redémarre
- [ ] Créer mon propre fork GitHub d'ARIES pour sauvegarder ce travail
