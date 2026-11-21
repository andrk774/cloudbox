// Persistent downloads, isolated by server + account. Partial files are never listed.
import 'dart:convert';
import 'dart:io';

import 'package:crypto/crypto.dart';
import 'package:path_provider/path_provider.dart';

import 'api.dart';

class OfflineEntry {
  final String id, name, path;
  final int size;
  final DateTime savedAt;
  OfflineEntry(this.id, this.name, this.path, this.size, this.savedAt);
}

class OfflineStore {
  final Directory directory;
  static Future<void> _tail = Future.value();
  OfflineStore(this.directory);

  static String accountKey(String server, String user) => sha256
      .convert(
        utf8.encode(
          '${CloudBoxApi.normalizeServer(server)}\n${user.toLowerCase()}',
        ),
      )
      .toString();

  static Future<OfflineStore> forAccount(String server, String user) async {
    final root = await getApplicationSupportDirectory();
    return OfflineStore(
      Directory('${root.path}/offline/${accountKey(server, user)}'),
    );
  }

  Future<T> _serial<T>(Future<T> Function() action) {
    final next = _tail.then((_) => action());
    _tail = next.then<void>((_) {}, onError: (Object _) {});
    return next;
  }

  String _key(String id) => sha256.convert(utf8.encode(id)).toString();

  Future<List<OfflineEntry>> list() => _serial(() async {
    if (!await directory.exists()) return [];
    final result = <OfflineEntry>[];
    await for (final item in directory.list()) {
      if (item is! File || !item.path.endsWith('.json')) continue;
      try {
        final j = jsonDecode(await item.readAsString()) as Map<String, dynamic>;
        final filename = j['file'] as String;
        // Metadata must never escape the private account directory.
        if (!RegExp(r'^[a-f0-9]+-\d+(\.[a-zA-Z0-9]{1,16})?$')
            .hasMatch(filename)) {
          continue;
        }
        final file = File('${directory.path}/$filename');
        final size = j['size'] as int;
        if (!await file.exists() || await file.length() != size) continue;
        result.add(
          OfflineEntry(
            j['id'],
            j['name'],
            file.path,
            size,
            DateTime.parse(j['saved_at']),
          ),
        );
      } on Object {
        /* Ignore damaged metadata; other offline files remain usable. */
      }
    }
    result.sort((a, b) => a.name.toLowerCase().compareTo(b.name.toLowerCase()));
    return result;
  });

  Future<void> save(Node node, Stream<List<int>> bytes) => _serial(() async {
    if (node.isDir || node.e2e) throw ArgumentError('Только обычные файлы');
    await directory.create(recursive: true);
    final key = _key(node.id);
    final ext =
        RegExp(r'\.[a-zA-Z0-9]{1,16}$').firstMatch(node.name)?.group(0) ?? '';
    final filename = '$key-${DateTime.now().microsecondsSinceEpoch}$ext';
    final data = File('${directory.path}/$filename');
    final partial = File('${data.path}.part');
    final meta = File('${directory.path}/$key.json');
    final pending = File('${meta.path}.part');
    try {
      final sink = await partial.open(mode: FileMode.write);
      try {
        await for (final chunk in bytes.timeout(const Duration(seconds: 60))) {
          await sink.writeFrom(chunk);
        }
      } finally {
        await sink.close();
      }
      if (await partial.length() != node.size) {
        throw const FileSystemException(
          'Файл изменился или загрузился не полностью. Обновите список и повторите.',
        );
      }
      await partial.rename(data.path);
      await pending.writeAsString(
        jsonEncode({
          'id': node.id,
          'name': node.name,
          'size': node.size,
          'file': filename,
          'saved_at': DateTime.now().toUtc().toIso8601String(),
        }),
        flush: true,
      );
      await pending.rename(meta.path);
    } catch (_) {
      for (final f in [partial, pending, data]) {
        if (await f.exists()) await f.delete();
      }
      rethrow;
    }
    // Remove superseded versions only after the new file and metadata are complete.
    await for (final f in directory.list()) {
      if (f is File &&
          f.uri.pathSegments.last.startsWith('$key-') &&
          f.uri.pathSegments.last != filename) {
        await f.delete();
      }
    }
  });

  Future<void> remove(String id) => _serial(() async {
    if (!await directory.exists()) return;
    final key = _key(id);
    await for (final f in directory.list()) {
      final name = f.uri.pathSegments.last;
      if (f is File && (name == '$key.json' || name.startsWith('$key-'))) {
        await f.delete();
      }
    }
  });

  Future<void> clear() => _serial(() async {
    if (await directory.exists()) await directory.delete(recursive: true);
  });
}
