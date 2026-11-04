// Клиент REST API CloudBox. Чистый Dart (без Flutter) — тестируется без устройства.
//
// Авторизация — Basic с паролем приложения (Настройки CloudBox → Пароли приложений):
// работает и при включённой двухфакторной аутентификации, отзывается на сайте в один клик.
import 'dart:async';
import 'dart:convert';
import 'dart:typed_data';

import 'package:http/http.dart' as http;

class ApiException implements Exception {
  final int status;
  final String message;
  ApiException(this.status, this.message);
  bool get unauthorized => status == 401;
  @override
  String toString() => message;
}

class User {
  final int id;
  final String username;
  final bool isAdmin;
  final int usedBytes;
  final int quotaBytes;
  User({required this.id, required this.username, required this.isAdmin, required this.usedBytes, required this.quotaBytes});
}

class Node {
  final String id;
  final String? parentId;
  final String name;
  final bool isDir;
  final int size;
  final String mime;
  final DateTime updatedAt;
  final bool e2e; // зашифрованная папка: её открывает только веб-интерфейс
  final bool favorite;
  Node({required this.id, this.parentId, required this.name, required this.isDir, required this.size, required this.mime,
      required this.updatedAt, this.e2e = false, this.favorite = false});

  factory Node.fromJson(Map<String, dynamic> j) => Node(
        id: j['id'] as String,
        parentId: j['parent_id'] as String?,
        name: j['name'] as String,
        isDir: j['is_dir'] as bool? ?? false,
        size: (j['size'] as num?)?.toInt() ?? 0,
        mime: j['mime'] as String? ?? '',
        updatedAt: DateTime.tryParse(j['updated_at'] as String? ?? '') ?? DateTime.fromMillisecondsSinceEpoch(0),
        e2e: j['e2e'] as bool? ?? false,
        favorite: j['favorite'] as bool? ?? false,
      );

  bool get isImage => mime.startsWith('image/');
  bool get isVideo => mime.startsWith('video/');
  bool get hasThumb => !isDir && !e2e && (isImage || isVideo || mime == 'application/pdf');
}

class Listing {
  final List<Node> items;
  final List<Node> breadcrumbs;
  final bool canWrite;
  Listing(this.items, this.breadcrumbs, this.canWrite);
}

const chunkSize = 8 << 20; // как в веб-интерфейсе и клиенте синхронизации

class CloudBoxApi {
  final Uri base;
  final String user;
  final String _password;
  final http.Client _http;

  CloudBoxApi(String server, this.user, this._password, {http.Client? client})
      : base = normalizeServer(server),
        _http = client ?? http.Client();

  /// https://cloud.example.ru/ → https://cloud.example.ru; без схемы — https.
  static Uri normalizeServer(String s) {
    var t = s.trim();
    if (!t.startsWith('http://') && !t.startsWith('https://')) t = 'https://$t';
    while (t.endsWith('/')) {
      t = t.substring(0, t.length - 1);
    }
    final u = Uri.parse(t);
    if (u.host.isEmpty) throw ApiException(0, 'Неверный адрес сервера');
    return u;
  }

  bool get insecure => base.scheme == 'http';

  Map<String, String> get authHeaders => {'Authorization': 'Basic ${base64.encode(utf8.encode('$user:$_password'))}'};

  Uri url(String path, [Map<String, String>? query]) =>
      base.replace(path: base.path + path, queryParameters: query == null || query.isEmpty ? null : query);

  Future<dynamic> _json(String method, String path, {Map<String, String>? query, Object? body}) async {
    final req = http.Request(method, url(path, query))..headers.addAll(authHeaders);
    if (body != null) {
      req.headers['Content-Type'] = 'application/json';
      req.body = jsonEncode(body);
    }
    final resp = await http.Response.fromStream(await _send(req));
    return _decode(resp);
  }

  Future<http.StreamedResponse> _send(http.BaseRequest req) async {
    try {
      return await _http.send(req).timeout(const Duration(seconds: 60));
    } on TimeoutException {
      throw ApiException(0, 'Сервер не отвечает');
    } catch (e) {
      if (e is ApiException) rethrow;
      throw ApiException(0, 'Нет связи с сервером: $e');
    }
  }

  dynamic _decode(http.Response resp) {
    dynamic data;
    try {
      data = resp.body.isEmpty ? null : jsonDecode(utf8.decode(resp.bodyBytes));
    } catch (_) {
      data = null;
    }
    if (resp.statusCode >= 400) {
      final msg = data is Map && data['error'] is String
          ? data['error'] as String
          : resp.statusCode == 401
              ? 'Неверное имя пользователя или пароль приложения'
              : 'Ошибка сервера ${resp.statusCode}';
      throw ApiException(resp.statusCode, msg);
    }
    return data;
  }

  // ---------- профиль ----------
  Future<User> me() async {
    final d = await _json('GET', '/api/me') as Map<String, dynamic>;
    final u = d['user'] as Map<String, dynamic>;
    return User(
      id: (u['id'] as num).toInt(),
      username: u['username'] as String,
      isAdmin: u['is_admin'] as bool? ?? false,
      usedBytes: (d['used_bytes'] as num?)?.toInt() ?? 0,
      quotaBytes: (d['quota_bytes'] as num?)?.toInt() ?? 0,
    );
  }

  // ---------- файлы ----------
  Future<Listing> list(String? parent) async {
    final d = await _json('GET', '/api/files', query: {'parent': ?parent}) as Map<String, dynamic>;
    List<Node> nodes(String k) => ((d[k] as List?) ?? []).map((e) => Node.fromJson(e as Map<String, dynamic>)).toList();
    return Listing(nodes('items'), nodes('breadcrumbs'), d['can_write'] as bool? ?? true);
  }

  Future<List<Node>> search(String q) async {
    final d = await _json('GET', '/api/search', query: {'q': q}) as List;
    return d.map((e) => Node.fromJson(e as Map<String, dynamic>)).toList();
  }

  Future<Node> mkdir(String? parent, String name) async =>
      Node.fromJson(await _json('POST', '/api/files/folder', body: {'parent': parent ?? '', 'name': name}) as Map<String, dynamic>);

  Future<Node> rename(String id, String name) async =>
      Node.fromJson(await _json('PATCH', '/api/files/$id', body: {'name': name}) as Map<String, dynamic>);

  Future<void> trash(String id) => _json('DELETE', '/api/files/$id');

  Uri thumbUrl(String id, int size) => url('/api/files/$id/thumb', {'s': '$size'});

  /// Скачивание потоком: chunks — части файла по мере поступления.
  Future<Stream<List<int>>> download(String id) async {
    final req = http.Request('GET', url('/api/files/$id/content'))..headers.addAll(authHeaders);
    final resp = await _send(req);
    if (resp.statusCode >= 400) _decode(await http.Response.fromStream(resp));
    return resp.stream;
  }

  /// Загрузка файла. read(offset, length) — чтение части (из файла на устройстве или из памяти).
  /// Маленькие файлы — одним запросом, большие — частями по 8 МБ с докачкой после обрыва.
  /// path — вложенные папки, создаются при необходимости ("Фото/Камера/2026").
  Future<Node> upload({
    required String? parent,
    required String name,
    required int size,
    required Future<Uint8List> Function(int offset, int length) read,
    String path = '',
    bool rename = true,
    void Function(double)? onProgress,
  }) async {
    final q = {'name': name, 'parent': ?parent, if (path.isNotEmpty) 'path': path, if (rename) 'conflict': 'rename'};
    if (size <= chunkSize) {
      final req = http.Request('POST', url('/api/files/upload', q))
        ..headers.addAll(authHeaders)
        ..bodyBytes = await read(0, size);
      final node = Node.fromJson(_decode(await http.Response.fromStream(await _send(req))) as Map<String, dynamic>);
      onProgress?.call(1);
      return node;
    }
    final up = await _json('POST', '/api/uploads', body: {
      'parent': parent ?? '', 'name': name, 'size': size, 'path': path, if (rename) 'conflict': 'rename',
    }) as Map<String, dynamic>;
    final id = up['id'] as String;
    var offset = (up['received'] as num?)?.toInt() ?? 0;
    var retries = 0;
    while (offset < size) {
      final len = (size - offset) < chunkSize ? size - offset : chunkSize;
      final req = http.Request('PUT', url('/api/uploads/$id', {'offset': '$offset'}))
        ..headers.addAll(authHeaders)
        ..bodyBytes = await read(offset, len);
      try {
        final d = _decode(await http.Response.fromStream(await _send(req))) as Map<String, dynamic>;
        offset = (d['received'] as num).toInt();
        retries = 0;
        onProgress?.call(offset / size);
      } on ApiException catch (e) {
        // обрыв связи или рассогласование смещения — спрашиваем сервер, сколько он получил, и продолжаем
        if (++retries > 5 || e.status == 401 || e.status == 413 || e.status == 507) rethrow;
        await Future<void>.delayed(Duration(seconds: retries * 2));
        final st = await _json('GET', '/api/uploads/$id') as Map<String, dynamic>;
        offset = (st['received'] as num).toInt();
      }
    }
    return Node.fromJson(await _json('POST', '/api/uploads/$id/complete') as Map<String, dynamic>);
  }

  void close() => _http.close();
}

String formatSize(int b) {
  const u = ['Б', 'КБ', 'МБ', 'ГБ', 'ТБ'];
  var v = b.toDouble();
  var i = 0;
  while (v >= 1024 && i < u.length - 1) {
    v /= 1024;
    i++;
  }
  return i == 0 ? '$b ${u[0]}' : '${v.toStringAsFixed(v < 10 ? 1 : 0)} ${u[i]}';
}
