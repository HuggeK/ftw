# Export FTW from a Home Assistant backup

This offline tool makes a separate copy of one FTW app's complete backup.
It keeps data, config, keys, drivers and app metadata. It checks protected
backups with Home Assistant's SecureTar library before extracting files.
It turns `/data` driver links into checked relative links. It never starts
Core, edits the original backup or replaces an existing destination.

Use a Linux computer with Python 3.11 or later and enough free space for the
extracted app. Keep the Home Assistant backup and its backup key. On the old
app, turn off **Start on boot** and **Watchdog**, then stop it before switching
equipment to the new Core. A downloaded backup alone does not stop the old app.

## Export

Use the exporter release package, or this directory from an exact FTW commit.
The package's `source.json` binds the scripts to that commit. Download the
separate tool beta into a new directory and check the archive:

```bash
mkdir ./ftw-ha-export-tool
cd ./ftw-ha-export-tool
tag=ha-export-v1-beta.1
base="https://github.com/srcfl/ftw/releases/download/$tag"
curl -fSL "$base/$tag.tar.gz" -o "$tag.tar.gz"
curl -fSL "$base/checksums.txt" -o checksums.txt
sha256sum -c checksums.txt
tar -xzf "$tag.tar.gz"
```

Stop if any command fails. Keep the tag and `source.json` with the export.
The tool has no build step:

```bash
python3 -m venv .venv
.venv/bin/python -m pip install --only-binary=:all: -r requirements.txt
.venv/bin/python ftw_ha_export.py /media/usb/ha-backup.tar --list
.venv/bin/python ftw_ha_export.py /media/usb/ha-backup.tar ./ftw-from-ha
```

The last command asks for the key when the backup is protected. To use a
local key file, add `--password-file /media/usb/ha-backup-key.txt`. Do not put
the key in the command line. Add `--slug <app slug>` if more than one FTW app
appears in the list. Add `--sha256 <expected checksum>` when you have a checked
checksum for the download.

The output directory has mode `0700`. `data/` contains the old FTW files;
`addon.json` and any app-specific config remain beside it. `ftw-export.json`
records the backup hash, source version, file hashes, old permissions and
checked links. Treat the whole directory as private. Normal file modes
remain; extracted files belong to the user who ran the tool. Hardlinks become
separate files with the same bytes. The original backup keeps the old owners
and original links for return to Home Assistant.

The tool supports standard Supervisor app layouts with `data/config.yaml`
and `data/state.db`, plain tar/gzip, SecureTar v2/v3 and old `snapshot.json`
metadata. It refuses missing files, duplicate file names, outside links,
cycles, device nodes and an existing destination. A failure leaves no selected
export. A killed process may leave a private `.ftw-ha-export-*` staging
directory; choose a new destination and rerun from the unchanged backup.
The tool does not turn an unknown community layout into a blank installation.

## Make a full FTW backup

The extracted settings still refer to the old app's `/data` directory. Keep
that root when running the published `ftw-backup create` helper. For example,
with a new FTW Docker image already built from its verified release package:

```bash
mkdir -p ./ftw-backups
docker run --rm --network none --user "$(id -u):$(id -g)" \
  -v "$PWD/ftw-from-ha/data:/data" \
  -v "$PWD/ftw-backups:/out" \
  --entrypoint /app/ftw-backup <your verified FTW image> \
  create -state /data/state.db -data /data -output /out \
  -core-version v3.8.0-beta.1 -progress
```

Replace the source version with the version printed by the export; it is not
the new helper's version. The extracted copy must be writable: SQLite may need
to create WAL/SHM sidecars while opening an old database. The original Home
Assistant backup stays unchanged. Use the helper's real path in your image. Do not
run `create -data ./ftw-from-ha/data` on the host and assume that old absolute
config paths have moved. The `/data` mount keeps that source root so verified
restore can map config and managed-driver choices to the target directory.

Use the resulting verified `.ftwbak` in the normal
[offline restore](https://github.com/srcfl/ftw/blob/master/docs/backup-and-restore.md#native-helper). Keep Core
stopped until the old app's automatic start paths are off. Check old history,
goals, driver choices, live readings and new writes before accepting the move.
Keep the original Home Assistant backup, its key and the matching old app image.
To return, stop and disable the new Core before restoring the old app backup.

This export tool checks file recovery. It does not prove a whole Supervisor
API/UI restore, every community layout or physical device control after a
switch. DuckDB sources still need the
[offline history converter](https://github.com/srcfl/ftw/blob/master/docs/history-conversion.md).
