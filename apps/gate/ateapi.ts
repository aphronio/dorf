// Agent Substrate's control API over gRPC. Every call carries its caller's own bearer token, so
// Substrate decides what the caller may do.
import grpc from "@grpc/grpc-js";
import loader from "@grpc/proto-loader";

const definition = loader.loadSync(new URL("./ateapi.proto", import.meta.url).pathname, { longs: String, enums: String, oneofs: true });
const { Control } = (grpc.loadPackageDefinition(definition) as any).substrate.ateapi.v1;

export type Api = (method: string, request: object, token: string) => Promise<any>;

// Long enough for a synchronous suspend to upload its snapshot.
const DEADLINE_MS = 5 * 60_000;

export function connect(address: string, ca: Buffer, serverName: string): Api {
	const client = new Control(address, grpc.credentials.createSsl(ca), { "grpc.ssl_target_name_override": serverName, "grpc.default_authority": serverName });
	return (method, request, token) =>
		new Promise((resolve, reject) => {
			const metadata = new grpc.Metadata();
			metadata.set("authorization", `Bearer ${token}`);
			client[method](request, metadata, { deadline: Date.now() + DEADLINE_MS }, (error: grpc.ServiceError | null, reply: unknown) =>
				error ? reject(error) : resolve(reply),
			);
		});
}

// gRPC status codes as HTTP statuses, for callers that speak HTTP.
export function httpStatus(error: { code?: number }): number {
	return ({ 3: 400, 5: 404, 6: 409, 7: 403, 9: 409, 16: 401 } as Record<number, number>)[error.code ?? -1] ?? 502;
}
