# Drime preview branch

This branch contains experimental improvements for rclone's Drime backend. It
is intended for Drime users who are affected by slow metadata operations,
directory creation races, pagination failures, or poor throughput with many
small files.

This is not an official rclone release and is not supported by the rclone or
Drime projects. Pin deployments to a tested commit and keep a copy of the
binary that created a remote.

## What this branch changes

- Uploads below 5 MiB can use presigned S3 URLs.
- Presign requests and subsequent entry creation can be batched.
- Multipart, presigned, and regular uploads use server-created relative paths
  where possible.
- Concurrent uploads coordinate missing path creation instead of racing to
  create the same directory.
- Uploaded content can be checked with Drime's server-side SHA-256 integrity
  endpoint.
- Recursive listings combine multiple parent IDs to reduce metadata requests.
- Recursive pagination detects repeated pages and continues through bounded
  creation-time windows.
- `rclone cleanup` can empty the Drime trash.

The batch endpoints used by this branch are not currently part of Drime's
published API documentation. Drime may change or remove them without notice.

## Important data-safety warning

Replacing an existing Drime file is currently unsafe.

In a controlled test, Drime successfully uploaded and indexed a replacement,
but invalidated the previous entry ID before rclone could delete it. The old
file disappeared from listings while its S3 object continued to consume
account storage. Deleting the replacement did not make the old file visible
again, and emptying the trash did not remove the orphaned object.

A related failure can occur when a multipart upload finishes but Drime rejects
entry creation because the parent ID no longer exists. At that point the bytes
may already be stored in S3 without a visible Drime entry. Automatic retries
can upload the same data again.

Until Drime fixes these server-side behaviours:

- prefer immutable destination paths;
- avoid overwriting existing files;
- do not use `--no-check-dest` unless the destination is known to be empty;
- initially use `--retries 1` when evaluating a new workload;
- stop a transfer if it reports an invalid entry ID or missing parent ID;
- compare Drime's reported storage usage with the visible data after failures;
- do not assume `rclone cleanup` can remove unindexed S3 objects.

## Configuration

Create a normal Drime remote and enable the preview features explicitly:

```ini
[drime]
type = drime
access_token = YOUR_ACCESS_TOKEN
use_presigned_uploads = true
presigned_upload_batch_size = 25
verify_uploads = true
list_parent_batch_size = 100
```

The same options work when a crypt remote wraps Drime:

```ini
[drime-crypt]
type = crypt
remote = drime:encrypted
password = YOUR_OBSCURED_PASSWORD
password2 = YOUR_OBSCURED_SALT
```

`verify_uploads` calculates SHA-256 while reading the upload and asks Drime to
verify the stored file after indexing. Drime does not expose that SHA-256 as a
normal rclone hash, so this option is independent of rclone's `--checksum`
flag.

## Initial transfer recommendations

Start with a small representative subset and detailed logging:

```bash
rclone -vv \
  --log-file rclone-drime.log \
  --retries 1 \
  --transfers 25 \
  --checkers 25 \
  copy /path/to/source drime:test-import
```

For workloads dominated by many independent files, increasing transfers and
checkers to 50 or 100 can improve throughput. Increase them gradually and
watch for HTTP 429 responses, metadata errors, and growing storage usage.

The presign and entry batch size remains 25 because Drime's entry-creation
batch accepts at most 25 files. Keeping `--transfers` at a multiple of 25 helps
fill the synchronous queues without adding unnecessary timeout latency.

For a known-empty destination, `--no-check-dest` avoids destination lookups,
but it must not be carried over to later incremental runs unless the caller has
an independently verified inventory.

## Building

Build with the normal rclone build process:

```bash
git clone https://github.com/BAERnado/rclone.git
cd rclone
git switch drime-preview
make
./rclone version
```

The version string includes the branch name and commit. Include the complete
output of `rclone version`, the command line, and a debug log when reporting a
problem.

## Reporting Drime upload failures

Useful evidence includes:

- exact UTC timestamp;
- rclone version and commit;
- whether the destination already existed;
- plaintext and remote sizes;
- Drime entry and parent IDs, if available;
- upload method: regular, presigned, or multipart;
- S3 key or multipart upload ID, if captured without exposing a signed URL;
- storage usage before and after the failure;
- whether emptying the trash changed the reported usage.

Never publish access tokens, crypt passwords, authorization headers, or full
presigned URLs. Presigned URL query parameters contain temporary credentials.

## Development workflow

`drime-preview` is the tested community branch. Experimental work should be
prepared on `drime-devel` and promoted only after backend tests and controlled
live tests succeed. Changes suitable for official rclone should continue to be
submitted as small, focused pull requests rather than as the complete preview
patch set.
