*This file is automatically generated via ./cmd/gendoc*

## Complement Configuration
Complement is configured exclusively through the use of environment variables. These variables are described below.

#### `COMPLEMENT_ALWAYS_PRINT_SERVER_LOGS`
If 1, always prints the Homeserver container logs even on success.  
- Type: `bool`
- Default: 0

#### `COMPLEMENT_BASE_IMAGE`
**Required.** The name of the Docker image to use as a base homeserver when generating blueprints. This image must conform to Complement's rules on containers, such as listening on the correct ports.  
- Type: `string`

#### `COMPLEMENT_BASE_IMAGE_*`
This allows you to override the base image used for a particular named homeserver. For example, `COMPLEMENT_BASE_IMAGE_HS1=complement-dendrite:latest` would use `complement-dendrite:latest` for the `hs1` homeserver in blueprints, but not any other homeserver (e.g `hs2`). This matching is case-insensitive. This allows Complement to test how different homeserver implementations work with each other.  
- Type: `map[string]string`

#### `COMPLEMENT_DEBUG`
If 1, prints out more verbose logging such as HTTP request/response bodies.  
- Type: `bool`
- Default: 0

#### `COMPLEMENT_HOSTNAME_RUNNING_COMPLEMENT`
The hostname of Complement from the perspective of a Homeserver running inside a container. This can be useful for container runtimes using another hostname to access the host from a container, like Podman that uses `host.containers.internal` instead.  
- Type: `string`
- Default: host.docker.internal

#### `COMPLEMENT_HOST_MOUNTS`
A list of semicolon separated host mounts to mount on every container. The structure of the mount is `host-path:container-path:[ro]` for example `/path/on/host:/path/on/container` - you can optionally specify `:ro` to mount the path as readonly. A complete example with multiple mounts would look like `/host/a:/container/a:ro;/host/b:/container/b;/host/c:/container/c`  
- Type: `[]HostMount`

#### `COMPLEMENT_KEEP_BLUEPRINTS`
A list of space separated blueprint names to not clean up after running. For example, `one_to_one_room alice` would not delete the homeserver images for the blueprints `alice` and `one_to_one_room`. This can speed up homeserver runs if you frequently run the same base image over and over again. If the base image changes, this should not be set as it means an older version of the base image will be used for the named blueprints.  
- Type: `[]string`

#### `COMPLEMENT_SHARE_ENV_PREFIX`
If set, all environment variables on the host with this prefix will be shared with every homeserver, with the prefix removed. For example, if the prefix was `FOO_` then setting `FOO_BAR=baz` on the host would translate to `BAR=baz` on the container. Useful for passing through extra Homeserver configuration options without sharing all host environment variables.  
- Type: `string`

#### `COMPLEMENT_SPAWN_HS_TIMEOUT_SECS`
The number of seconds to wait for a Homeserver container to be responsive after starting the container. Responsiveness is detected by `HEALTHCHECK` being healthy *and* the `/versions` endpoint returning 200 OK.  
- Type: `Duration`
- Default: 30
