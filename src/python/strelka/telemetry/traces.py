from opentelemetry import trace
from opentelemetry.exporter.jaeger.thrift import JaegerExporter
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import BatchSpanProcessor

from . import resource

jaeger_exporter = JaegerExporter(
    agent_host_name="strelka_jaeger_1",
    agent_port=6831,
)

provider = TracerProvider(resource=resource)
processor = BatchSpanProcessor(jaeger_exporter)  # ConsoleSpanExporter())
provider.add_span_processor(processor)

# Sets the global default tracer provider
trace.set_tracer_provider(provider)

# Creates a tracer from the global tracer provider
tracer = trace.get_tracer(__name__)


def my_decorator_func(func):
    def wrapper_func(*args, **kwargs):
        # Do something before the function.
        func(*args, **kwargs)
        # Do something after the function.

    return wrapper_func
