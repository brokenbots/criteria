import * as net from "net";
import * as tls from "tls";
import * as path from "path";
import * as fs from "fs";
import { pipeline } from "stream/promises";
import * as grpc from "@grpc/grpc-js";
import * as protoLoader from "@grpc/proto-loader";

// ─── Types ──────────────────────────────────────────────────────────────────

export interface RemoteIdentity {
  name: string;
  version: string;
  digest: string;
}

export interface ServeRemoteOptions {
  host: string;
  tls?: tls.ConnectionOptions;
  identity: RemoteIdentity;
  acceptToken?: string;
  /** Internal Unix socket path; defaults to a temporary path. */
  socketPath?: string;
}

export interface Service {
  info(call: grpc.ServerUnaryCall<any, any>, callback: grpc.sendUnaryData<any>): void;
  openSession(call: grpc.ServerUnaryCall<any, any>, callback: grpc.sendUnaryData<any>): void;
  execute(call: grpc.ServerReadableStream<any, any>, callback: grpc.sendUnaryData<any>): void;
  log(call: grpc.ServerReadableStream<any, any>, callback: grpc.sendUnaryData<any>): void;
  permissions(call: grpc.ServerDuplexStream<any, any>): void;
  closeSession(call: grpc.ServerUnaryCall<any, any>, callback: grpc.sendUnaryData<any>): void;
}

// ─── Proto loading ──────────────────────────────────────────────────────────

function findProtoRoot(): string {
  let dir = __dirname;
  for (let i = 0; i < 5; i++) {
    const candidate = path.join(dir, "proto");
    if (fs.existsSync(path.join(candidate, "criteria", "v2", "adapter.proto"))) {
      return candidate;
    }
    const parent = path.dirname(dir);
    if (parent === dir) break;
    dir = parent;
  }
  throw new Error("cannot locate proto/criteria/v2/adapter.proto relative to " + __dirname);
}

const PROTO_ROOT = findProtoRoot();

const packageDefinition = protoLoader.loadSync(
  path.join(PROTO_ROOT, "criteria", "v2", "adapter.proto"),
  {
    keepCase: true,
    longs: String,
    enums: String,
    defaults: true,
    oneofs: true,
    includeDirs: [PROTO_ROOT],
  }
);

const protoDescriptor = grpc.loadPackageDefinition(packageDefinition) as any;
const adapterService = protoDescriptor.criteria.v2.AdapterService.service as grpc.ServiceDefinition<any>;

// ─── Helpers ────────────────────────────────────────────────────────────────

async function dialRemote(host: string, tlsOpts?: tls.ConnectionOptions): Promise<net.Socket> {
  if (path.isAbsolute(host) || host.startsWith("/")) {
    return net.createConnection(host);
  }
  if (tlsOpts) {
    return tls.connect({ ...tlsOpts, host: host.split(":")[0], port: parseInt(host.split(":")[1] || "443", 10) });
  }
  const [h, p] = host.split(":");
  return net.createConnection({ host: h, port: parseInt(p || "443", 10) });
}

async function sendHandshake(conn: net.Socket, identity: RemoteIdentity, token?: string): Promise<void> {
  const msg = {
    name: identity.name,
    version: identity.version,
    digest: identity.digest,
    token: token || undefined,
    sdk_protocol_version: 2,
  };
  const line = JSON.stringify(msg) + "\n";
  return new Promise((resolve, reject) => {
    conn.write(line, (err) => {
      if (err) reject(err);
      else resolve();
    });
  });
}

function pickUnixSocketPath(): string {
  // Use a temp path in /tmp to avoid length limits.
  return `/tmp/criteria-ts-adapter-${process.pid}-${Date.now()}.sock`;
}

// ─── Bridge ─────────────────────────────────────────────────────────────────

async function bridgeSockets(a: net.Socket, b: net.Socket): Promise<void> {
  await Promise.all([
    pipeline(a, b).catch((err) => {
      if ((err as Error)?.message !== "The operation was aborted") {
        // eslint-disable-next-line no-console
        console.error("bridge error:", err);
      }
    }),
    pipeline(b, a).catch((err) => {
      if ((err as Error)?.message !== "The operation was aborted") {
        // eslint-disable-next-line no-console
        console.error("bridge error:", err);
      }
    }),
  ]);
}

// ─── serveRemote ────────────────────────────────────────────────────────────

export async function serveRemote(service: Service, opts: ServeRemoteOptions): Promise<void> {
  if (!opts.host) {
    throw new Error("serveRemote: host is required");
  }

  const unixPath = opts.socketPath || pickUnixSocketPath();

  const server = new grpc.Server();
  server.addService(adapterService, {
    info: service.info.bind(service),
    openSession: (service as any).openSession?.bind(service),
    execute: (service as any).execute?.bind(service),
    log: (service as any).log?.bind(service),
    permissions: (service as any).permissions?.bind(service),
    closeSession: (service as any).closeSession?.bind(service),
  } as any);

  await new Promise<void>((resolve, reject) => {
    server.bindAsync(
      `unix://${unixPath}`,
      grpc.ServerCredentials.createInsecure(),
      (err) => {
        if (err) reject(err);
        else resolve();
      }
    );
  });

  let conn: net.Socket | undefined;
  let local: net.Socket | undefined;
  try {
    conn = await dialRemote(opts.host, opts.tls);
    await sendHandshake(conn, opts.identity, opts.acceptToken);
  } catch (err) {
    conn?.destroy();
    server.forceShutdown();
    throw new Error(`serveRemote: handshake failed: ${err}`);
  }

  local = net.createConnection(unixPath);

  // When either side closes, clean up the server and socket.
  conn.on("close", () => {
    local.destroy();
    server.forceShutdown();
  });
  local.on("close", () => {
    conn.destroy();
    server.forceShutdown();
  });

  await bridgeSockets(conn, local);
}
