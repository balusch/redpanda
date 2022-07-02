// Copyright 2020 Redpanda Data, Inc.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.md
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0

#include "net/connection.h"

#include "rpc/service.h"

namespace net {

/**
 * If the exception is a "boring" disconnection case, then populate this with
 * the reason.
 *
 * This avoids logging overly alarmist "error" messages for exceptions that
 * are typical in the case of a client or node simply stopping.
 */
std::optional<ss::sstring> is_disconnect_exception(std::exception_ptr e) {
    try {
        rethrow_exception(e);
    } catch (std::system_error& e) {
        if (
          e.code() == std::errc::broken_pipe
          || e.code() == std::errc::connection_reset
          || e.code() == std::errc::connection_aborted) {
            return e.code().message();
        }
    } catch (const net::batched_output_stream_closed& e) {
        return "stream closed";
    } catch (const std::out_of_range&) {
        // Happens on unclean client disconnect, when io_iterator_consumer
        // gets fewer bytes than it wanted
        return "short read";
    } catch (const rpc::rpc_internal_body_parsing_exception&) {
        // Happens on unclean client disconnect, typically wrapping
        // an out_of_range
        return "parse error";
    } catch (...) {
        // Global catch-all prevents stranded/non-handled exceptional futures.
        // In all other non-explicity handled cases, the exception will not be
        // related to disconnect issues, therefore fallthrough to return nullopt
        // is acceptable.
    }

    return std::nullopt;
}

connection::connection(
  boost::intrusive::list<connection>& hook,
  ss::sstring name,
  ss::connected_socket f,
  ss::socket_address a,
  server_probe& p,
  std::optional<size_t> in_max_buffer_size)
  : addr(a)
  , _hook(hook)
  , _name(std::move(name))
  , _fd(std::move(f))
  , _in(_fd.input())
  // balus(N): net::batched_output_stream 接受一个 ss::output_stream +
  // 一个默认参数， 所以这里才可以直接赋值
  , _out(_fd.output())
  // balus(N):
  // 类里面的引用类型成员，需要保证其引用的对象在该类之后才被释放，对于
  // connection，其引用的 _hook/_probe 都是其所属 server 中的成员，而 server
  // 一定会确保在所有 connection 都被清理之后才释放
  // balus(T): 不过 server 仅仅是用 boost::intrusive_list 保存所有的
  // connection，它并不管理 connection 的生命周期(不像
  // std::list)，那么它是怎么来确保上面提到的这点呢？其实不是通过
  // _hook，而是通过 server 中的 _gate，每个创建出来的 connection 必定属于某个
  // accept 异步操作(server::accept() 函数，lambda
  // 捕获)，只不过为了便于管理才加入到 _hook 链表中去的，异步操作结束这个
  // connection 就自动释放并从链表中脱离了(其析构函数的行为)，当关闭 server
  // 时，server 通过 _gate 确保在所有它发起的后台 accept 操作都结束之后才清理
  // server，这样就确保了上一点
  , _probe(p) {
>>>>>>> 3f2e07f39 (Study net/connection.)
    if (in_max_buffer_size.has_value()) {
        auto in_config = ss::connected_socket_input_stream_config{};
        in_config.max_buffer_size = in_max_buffer_size.value();
        _in = _fd.input(std::move(in_config));
    } else {
        _in = _fd.input();
    }

    _hook.push_back(*this);
    _probe.connection_established();
}

connection::~connection() noexcept { _hook.erase(_hook.iterator_to(*this)); }

void connection::shutdown_input() {
    try {
        _fd.shutdown_input();
    } catch (...) {
        _probe.connection_close_error();
        rpc::rpclog.debug(
          "Failed to shutdown connection: {}", std::current_exception());
    }
}

ss::future<> connection::shutdown() {
    _probe.connection_closed();
    return _out.stop();
}

ss::future<> connection::write(ss::scattered_message<char> msg) {
    _probe.add_bytes_sent(msg.size());
    return _out.write(std::move(msg));
}

} // namespace net
